package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	// No dca library import should be here
)

// Configuration Variables - will be loaded from .env or defaults
var (
	botToken          string
	serverID          string
	voiceChannelID    string
	notificationSound string
	webSocketURL      string

	isPlayingSound    bool
	playSoundMutex    sync.Mutex
	discordSession    *discordgo.Session
	wsConnection      *websocket.Conn
	reconnectAttempts = 0
	dcaSoundBuffer    = make([][]byte, 0) // Buffer for pre-encoded DCA sound
)

// WebSocketMessage struct remains the same
type WebSocketMessage struct {
	TCIN              string  `json:"tcin"`
	Title             string  `json:"title"`
	AvailableQuantity float64 `json:"availableQuantity"`
	ProductURL        string  `json:"productURL"`
}

// getEnvVoiceBot helper function to get environment variables with a fallback default
func getEnvVoiceBot(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		log.Printf("VOICEBOT_CONFIG: Loaded '%s' from environment.", key)
		return value
	}
	log.Printf("VOICEBOT_CONFIG: '%s' not found in environment, using default: '%s'", key, fallback)
	return fallback
}

func main() {
	// Load environment variables from .env file in the parent directory
	err := godotenv.Load("../.env") // Assumes .env is in targay/
	if err != nil {
		log.Printf("VOICEBOT_CONFIG: Error loading .env file from ../.env: %v. Will rely on existing environment variables or defaults.", err)
		// This is not necessarily fatal if env vars are set in the system, or defaults are acceptable.
	}

	// Populate configuration variables
	botToken = os.Getenv("VOICEBOT_TOKEN")                                            // No fallback, critical
	serverID = getEnvVoiceBot("VOICEBOT_SERVER_ID", "")                               // Provide your default server ID if desired
	voiceChannelID = getEnvVoiceBot("VOICEBOT_CHANNEL_ID", "")                        // Provide your default channel ID if desired
	notificationSound = getEnvVoiceBot("VOICEBOT_SOUND_FILE", "noti.dca")             // Default to noti.dca if not set
	webSocketURL = getEnvVoiceBot("VOICEBOT_WEBSOCKET_URL", "ws://localhost:6969/ws") // Default WebSocket URL

	if botToken == "" {
		log.Fatalf("CRITICAL: VOICEBOT_TOKEN is not set in environment or .env file. Bot cannot start.")
	}
	if serverID == "" {
		log.Println("WARN: VOICEBOT_SERVER_ID is not set. Bot may not function correctly for specific guild operations.")
	}
	if voiceChannelID == "" {
		log.Println("WARN: VOICEBOT_CHANNEL_ID is not set. Bot will not be able to join a voice channel automatically.")
	}

	// Load the .dca sound file at startup
	err = loadSoundDCA(notificationSound)
	if err != nil {
		log.Fatalf("CRITICAL: Error loading sound file '%s': %v\nPlease ensure noti.dca exists and is correctly formatted.", notificationSound, err)
	}
	if len(dcaSoundBuffer) == 0 {
		log.Fatalf("CRITICAL: Sound buffer is empty after loading '%s'. Check noti.dca content.", notificationSound)
	}
	log.Printf("SOUND_LOAD: Successfully loaded %d Opus frames from '%s'", len(dcaSoundBuffer), notificationSound)

	discordSession, err = discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}
	discordSession.AddHandler(ready)
	discordSession.AddHandler(voiceStateUpdate)
	discordSession.Identify.Intents = discordgo.IntentsGuildVoiceStates
	err = discordSession.Open()
	if err != nil {
		log.Fatalf("Error opening connection to Discord: %v", err)
	}
	defer discordSession.Close()
	log.Println("Discord Voice Bot (DCA Player) is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	log.Println("Shutting down Discord Voice Bot (DCA Player)...")
	if wsConnection != nil {
		wsConnection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		wsConnection.Close()
	}
}

func loadSoundDCA(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening dca file '%s': %w", path, err)
	}
	defer file.Close()
	dcaSoundBuffer = make([][]byte, 0) // Clear buffer before loading
	var opuslen int16
	for {
		err = binary.Read(file, binary.LittleEndian, &opuslen)
		if err == io.EOF {
			return nil
		}
		if err == io.ErrUnexpectedEOF {
			log.Printf("WARN_DCA_LOAD: Unexpected EOF from '%s'. File might be empty/truncated.", path)
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading frame length from '%s': %w", path, err)
		}
		if opuslen <= 0 {
			return fmt.Errorf("invalid frame length %d in '%s'", opuslen, path)
		}
		InBuf := make([]byte, opuslen)
		bytesRead, readErr := io.ReadFull(file, InBuf)
		if readErr != nil {
			return fmt.Errorf("reading frame data (read %d, expected %d) from '%s': %w", bytesRead, opuslen, path, readErr)
		}
		dcaSoundBuffer = append(dcaSoundBuffer, InBuf)
	}
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v (ID: %s)", s.State.User.Username, s.State.User.Discriminator, s.State.User.ID)
	s.UpdateGameStatus(0, "Listening for stock...")
	go connectToWebSocket()
}

func connectToWebSocket() {
	log.Printf("Attempting to connect to WebSocket: %s", webSocketURL)
	var err error
	wsConnection, _, err = websocket.DefaultDialer.Dial(webSocketURL, nil)
	if err != nil {
		log.Printf("WebSocket dial error: %v", err)
		reconnectAttempts++
		wait := time.Duration(min(30, reconnectAttempts*5)) * time.Second
		log.Printf("Will retry WebSocket connection in %s...", wait)
		time.Sleep(wait)
		connectToWebSocket()
		return
	}
	log.Println("WebSocket connected successfully.")
	reconnectAttempts = 0
	listenForStockAlerts(wsConnection)
	log.Println("WebSocket connection lost. Reconnecting...")
	if wsConnection != nil {
		wsConnection.Close()
	}
	connectToWebSocket()
}

func listenForStockAlerts(conn *websocket.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Recovered in listenForStockAlerts:", r)
		}
		log.Println("Stopped listening for stock alerts (conn closed or error).")
	}()
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket ReadMessage error: %v", err)
			return
		}
		var stockAlert WebSocketMessage
		if err := json.Unmarshal(message, &stockAlert); err != nil {
			log.Printf("JSON Unmarshal error: %v. Message: '%s'", err, string(message))
			continue
		}
		log.Printf("High stock alert: TCIN %s - '%s' (Qty: %.0f)", stockAlert.TCIN, stockAlert.Title, stockAlert.AvailableQuantity)
		go func(alert WebSocketMessage) {
			playSoundMutex.Lock()
			if isPlayingSound {
				log.Println("DCA_PLAY: Sound already playing, skipping for TCIN", alert.TCIN)
				playSoundMutex.Unlock()
				return
			}
			isPlayingSound = true
			playSoundMutex.Unlock()
			var vc *discordgo.VoiceConnection
			defer func() {
				playSoundMutex.Lock()
				isPlayingSound = false
				playSoundMutex.Unlock()
				if vc != nil {
					vc.Disconnect()
				}
				log.Println("DCA_PLAY: Goroutine finished for TCIN", alert.TCIN)
			}()
			log.Printf("DCA_PLAY: Joining channel %s for TCIN %s", voiceChannelID, alert.TCIN)
			vc, err = discordSession.ChannelVoiceJoin(serverID, voiceChannelID, false, false)
			if err != nil {
				log.Printf("ERROR_DCA_PLAY: Join error for TCIN %s: %v", alert.TCIN, err)
				return
			}
			playSoundFromBuffer(vc, alert.TCIN)
		}(stockAlert)
	}
}

// playSoundFromBuffer sends pre-loaded Opus frames from dcaSoundBuffer to the voice connection.
func playSoundFromBuffer(vc *discordgo.VoiceConnection, tcinForLog string) {
	log.Printf("playSoundFromBuffer: Playing for TCIN %s. Buffer size: %d frames.", tcinForLog, len(dcaSoundBuffer))
	if vc == nil {
		log.Println("ERROR_DCA_PLAY: Voice connection is nil.")
		return
	}
	// Wait for VC to be ready
	ready := false
	for i := 0; i < 30; i++ { // Try for up to 3 seconds
		if vc.Ready {
			log.Println("DCA_PLAY_DEBUG: Voice connection is now ready.")
			ready = true
			break
		}
		log.Println("DCA_PLAY_DEBUG: Voice connection not ready, waiting 100ms...")
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		log.Println("ERROR_DCA_PLAY: Voice connection did not become ready in time.")
		return
	}

	time.Sleep(100 * time.Millisecond) // Short pause before speaking

	if err := vc.Speaking(true); err != nil {
		log.Printf("ERROR_DCA_PLAY: vc.Speaking(true) error: %v", err)
		return
	}
	defer vc.Speaking(false)

	log.Println("DCA_PLAY_DEBUG: Sending Opus frames...")
	for _, frame := range dcaSoundBuffer {
		select {
		case vc.OpusSend <- frame:
		case <-time.After(1 * time.Second): // Timeout for sending a single frame
			log.Println("ERROR_DCA_PLAY: Timeout sending Opus frame.")
			return // Exit if send times out
		}
	}
	log.Printf("DCA_PLAY_DEBUG: Finished sending frames for TCIN %s.", tcinForLog)
	time.Sleep(100 * time.Millisecond) // Brief pause after sending all frames
}

func voiceStateUpdate(s *discordgo.Session, vsu *discordgo.VoiceStateUpdate) {
	if vsu.UserID == s.State.User.ID {
		log.Printf("VOICE_STATE: ChanID: '%s', GuildID: '%s', SelfDeaf: %t, SelfMute: %t",
			vsu.ChannelID, vsu.GuildID, vsu.SelfDeaf, vsu.SelfMute)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
