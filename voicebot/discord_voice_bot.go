package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
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
	// dcaSoundBuffer    = make([][]byte, 0) // Buffer for pre-encoded DCA sound - REMOVED

	// Google Cloud TTS specific
	googleCloudProjectID   string
	googleCloudAccessToken string // Consider secure storage and refresh mechanisms for production
	accessTokenMutex       sync.Mutex
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
	botToken = os.Getenv("VOICEBOT_TOKEN")                     // No fallback, critical
	serverID = getEnvVoiceBot("VOICEBOT_SERVER_ID", "")        // Provide your default server ID if desired
	voiceChannelID = getEnvVoiceBot("VOICEBOT_CHANNEL_ID", "") // Provide your default channel ID if desired
	// notificationSound = getEnvVoiceBot("VOICEBOT_SOUND_FILE", "noti.dca")             // Default to noti.dca if not set - REMOVED
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

	// Initialize Google Cloud configuration
	if err := initializeGoogleCloudConfig(); err != nil {
		log.Fatalf("CRITICAL: Failed to initialize Google Cloud config: %v", err)
	}
	log.Println("GOOGLE_CLOUD_CONFIG: Successfully initialized Google Cloud Project ID.")
	// Fetch initial access token
	if err := refreshGoogleCloudAccessToken(); err != nil {
		log.Printf("WARN: Failed to fetch initial Google Cloud access token: %v. Will attempt later.", err)
		// Depending on your tolerance, this could be fatal.
		// For now, we'll let it try again on first use.
	} else {
		log.Println("GOOGLE_CLOUD_CONFIG: Successfully fetched initial Google Cloud access token.")
	}

	// Load the .dca sound file at startup - REMOVED
	// err = loadSoundDCA(notificationSound)
	// if err != nil {
	// 	log.Fatalf("CRITICAL: Error loading sound file '%s': %v\nPlease ensure noti.dca exists and is correctly formatted.", notificationSound, err)
	// }
	// if len(dcaSoundBuffer) == 0 {
	// 	log.Fatalf("CRITICAL: Sound buffer is empty after loading '%s'. Check noti.dca content.", notificationSound)
	// }
	// log.Printf("SOUND_LOAD: Successfully loaded %d Opus frames from '%s'", len(dcaSoundBuffer), notificationSound)

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

// loadSoundDCA removed as we will generate audio dynamically.
/*
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
*/

// --- Google Cloud TTS Functions ---

func initializeGoogleCloudConfig() error {
	cmd := exec.Command("gcloud", "config", "list", "--format=value(core.project)")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("gcloud config list error: %v, stderr: %s", err, stderr.String())
	}
	projectID := strings.TrimSpace(out.String())
	if projectID == "" {
		return fmt.Errorf("gcloud config list returned empty project ID")
	}
	googleCloudProjectID = projectID
	log.Printf("GOOGLE_CLOUD_CONFIG: Project ID set to '%s'", googleCloudProjectID)
	return nil
}

func getGoogleCloudAccessTokenCmd() (string, error) {
	cmd := exec.Command("gcloud", "auth", "print-access-token")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("gcloud auth print-access-token error: %v, stderr: %s", err, stderr.String())
	}
	token := strings.TrimSpace(out.String())
	if token == "" {
		return "", fmt.Errorf("gcloud auth print-access-token returned empty token")
	}
	return token, nil
}

func refreshGoogleCloudAccessToken() error {
	accessTokenMutex.Lock()
	defer accessTokenMutex.Unlock()
	token, err := getGoogleCloudAccessTokenCmd()
	if err != nil {
		return fmt.Errorf("failed to get new access token: %w", err)
	}
	googleCloudAccessToken = token
	log.Println("GOOGLE_CLOUD_CONFIG: Access token refreshed.")
	return nil
}

func getActiveToken() (string, error) {
	accessTokenMutex.Lock()
	defer accessTokenMutex.Unlock()
	if googleCloudAccessToken == "" {
		// Attempt to refresh if it's empty (e.g., initial fetch failed)
		log.Println("GOOGLE_CLOUD_CONFIG: Access token is empty, attempting to refresh...")
		err := refreshGoogleCloudAccessToken() // This will call Lock again, but it's fine due to defer
		if err != nil {
			return "", fmt.Errorf("tried to refresh empty token but failed: %w", err)
		}
	}
	return googleCloudAccessToken, nil
}

// synthesizeSpeechGoogleCloud sends a request to Google Cloud TTS API and returns MP3 audio data.
func synthesizeSpeechGoogleCloud(textToSpeak string) ([]byte, error) {
	token, err := getActiveToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get active access token for TTS: %w", err)
	}

	if googleCloudProjectID == "" {
		return nil, fmt.Errorf("Google Cloud Project ID is not set")
	}

	requestURL := "https://texttospeech.googleapis.com/v1/text:synthesize"
	requestBody := map[string]interface{}{
		"input": map[string]string{
			// Using "text" instead of "markup" for simplicity, adjust if SSML is needed.
			"text": textToSpeak,
		},
		"voice": map[string]interface{}{
			"languageCode": "en-US",
			"name":         "en-US-Chirp3-HD-Kore", // As per user's example
			// "voiceClone":   map[string]interface{}{}, // Removed unless voice cloning is set up and intended
		},
		"audioConfig": map[string]interface{}{
			"audioEncoding": "MP3", // Requesting MP3 output
			"speakingRate":  1.35,  // As per user's example
		},
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TTS request body: %w", err)
	}

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create TTS request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-User-Project", googleCloudProjectID)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second} // Increased timeout for API call + audio transfer
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send TTS request: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read TTS response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Check for token expiry, attempt refresh once
		if resp.StatusCode == http.StatusUnauthorized {
			log.Printf("WARN_TTS: Received 401 Unauthorized. Attempting to refresh access token and retry TTS for '%s'.", textToSpeak)
			errRefresh := refreshGoogleCloudAccessToken()
			if errRefresh != nil {
				return nil, fmt.Errorf("TTS request failed with status %d (body: %s), and token refresh also failed: %w", resp.StatusCode, string(responseBody), errRefresh)
			}
			// Retry the request with the new token
			token, err = getActiveToken() // get the newly refreshed token
			if err != nil {
				return nil, fmt.Errorf("failed to get newly refreshed active access token for TTS retry: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)   // Update header
			req.Body = io.NopCloser(bytes.NewBuffer(jsonData)) // Reset body for retry

			respRetry, errRetry := client.Do(req)
			if errRetry != nil {
				return nil, fmt.Errorf("failed to send TTS request on retry: %w", errRetry)
			}
			defer respRetry.Body.Close()
			responseBodyRetry, errReadRetry := io.ReadAll(respRetry.Body)
			if errReadRetry != nil {
				return nil, fmt.Errorf("failed to read TTS response body on retry: %w", errReadRetry)
			}
			if respRetry.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("TTS request failed on retry with status %d: %s", respRetry.StatusCode, string(responseBodyRetry))
			}
			responseBody = responseBodyRetry // Use the successful retry response
		} else {
			return nil, fmt.Errorf("TTS request failed with status %d: %s", resp.StatusCode, string(responseBody))
		}
	}

	// Assuming the response body for MP3 is a JSON with "audioContent" field containing base64 encoded audio
	var ttsResponse struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(responseBody, &ttsResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal TTS response JSON (is it base64 audio content?): %w. Body: %s", err, string(responseBody))
	}

	audioData, err := base64.StdEncoding.DecodeString(ttsResponse.AudioContent)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 audio content from TTS response: %w", err)
	}
	log.Printf("TTS_SUCCESS: Synthesized speech for '%s', MP3 size: %d bytes", textToSpeak, len(audioData))
	return audioData, nil
}

// convertMP3ToDCAFrames converts MP3 audio data (in-memory) to DCA Opus frames using ffmpeg and a 'dca' CLI tool.
func convertMP3ToDCAFrames(mp3Data []byte, productNameForLog string) ([][]byte, error) {
	log.Printf("CONVERT_AUDIO: Starting MP3 to DCA conversion for '%s'", productNameForLog)

	// ffmpeg command to convert MP3 from stdin to s16le PCM to stdout
	// ffmpeg -i pipe:0 -f s16le -ar 48000 -ac 2 pipe:1
	ffmpegCmd := exec.Command("ffmpeg", "-i", "pipe:0", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	ffmpegCmd.Stdin = bytes.NewReader(mp3Data) // Provide MP3 data to ffmpeg's stdin

	var ffmpegStderr bytes.Buffer
	ffmpegCmd.Stderr = &ffmpegStderr

	// dca command to convert s16le PCM from stdin to DCA Opus frames to stdout
	// dca (or your dca tool equivalent)
	dcaCmd := exec.Command("dca") // Assumes 'dca' tool is in PATH and works like: cat audio.s16le | dca > audio.dca
	var dcaStderr bytes.Buffer
	dcaCmd.Stderr = &dcaStderr

	// Pipe ffmpeg's stdout to dca's stdin
	pipeReader, pipeWriter := io.Pipe()
	ffmpegCmd.Stdout = pipeWriter
	dcaCmd.Stdin = pipeReader

	// Capture dca's stdout (this will be the DCA formatted Opus frames)
	var dcaOutput bytes.Buffer
	dcaCmd.Stdout = &dcaOutput

	// Start dca command first, then ffmpeg
	if err := dcaCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start 'dca' command: %w. Stderr: %s", err, dcaStderr.String())
	}
	log.Printf("CONVERT_AUDIO_DEBUG: 'dca' command started for '%s'", productNameForLog)

	if err := ffmpegCmd.Start(); err != nil {
		pipeWriter.CloseWithError(err) // Close pipe if ffmpeg fails to start
		dcaCmd.Wait()                  // Wait for dca to finish
		return nil, fmt.Errorf("failed to start 'ffmpeg' command: %w. Stderr: %s", err, ffmpegStderr.String())
	}
	log.Printf("CONVERT_AUDIO_DEBUG: 'ffmpeg' command started for '%s'", productNameForLog)

	// Goroutine to close the pipe writer once ffmpeg is done.
	// This signals EOF to the dca command's stdin.
	var ffmpegErr error
	go func() {
		ffmpegErr = ffmpegCmd.Wait()
		pipeWriter.Close() // Close the writer end of the pipe
		if ffmpegErr != nil {
			log.Printf("ERROR_CONVERT_AUDIO: ffmpeg failed for '%s': %v. Stderr: %s", productNameForLog, ffmpegErr, ffmpegStderr.String())
		} else {
			log.Printf("CONVERT_AUDIO_DEBUG: ffmpeg finished successfully for '%s'", productNameForLog)
		}
	}()

	dcaErr := dcaCmd.Wait()
	if dcaErr != nil {
		// ffmpegErr might also be set if ffmpeg failed
		return nil, fmt.Errorf("dca command failed for '%s': %w. Stderr: %s. (ffmpeg error if any: %v)", productNameForLog, dcaErr, dcaStderr.String(), ffmpegErr)
	}
	if ffmpegErr != nil { // Check ffmpeg error again in case dca finished "successfully" but ffmpeg had issues
		return nil, fmt.Errorf("ffmpeg command failed during conversion for '%s': %w. Stderr: %s", productNameForLog, ffmpegErr, ffmpegStderr.String())
	}

	log.Printf("CONVERT_AUDIO_DEBUG: 'dca' command finished for '%s'. Output size: %d bytes.", productNameForLog, dcaOutput.Len())

	// Now, parse the DCA output (which is a stream of length-prefixed Opus frames)
	// This is similar to how loadSoundDCA worked.
	var soundFrames [][]byte
	dcaDataReader := bytes.NewReader(dcaOutput.Bytes())
	var opuslen int16

	for {
		err := binary.Read(dcaDataReader, binary.LittleEndian, &opuslen)
		if err == io.EOF {
			break // End of DCA stream
		}
		if err == io.ErrUnexpectedEOF {
			log.Printf("WARN_CONVERT_AUDIO: Unexpected EOF while reading Opus frame length from DCA output for '%s'. Frames parsed so far: %d", productNameForLog, len(soundFrames))
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading Opus frame length from DCA output for '%s': %w", productNameForLog, err)
		}
		if opuslen <= 0 {
			return nil, fmt.Errorf("invalid Opus frame length %d in DCA output for '%s'", opuslen, productNameForLog)
		}

		frameData := make([]byte, opuslen)
		bytesRead, readErr := io.ReadFull(dcaDataReader, frameData)
		if readErr != nil {
			return nil, fmt.Errorf("error reading Opus frame data (read %d, expected %d) from DCA output for '%s': %w", bytesRead, opuslen, productNameForLog, readErr)
		}
		soundFrames = append(soundFrames, frameData)
	}

	if len(soundFrames) == 0 {
		log.Printf("WARN_CONVERT_AUDIO: No Opus frames extracted from DCA output for '%s'. ffmpeg stderr: %s, dca stderr: %s", productNameForLog, ffmpegStderr.String(), dcaStderr.String())
		// Return an error or empty slice depending on desired behavior for silent failure.
		// For now, let's return an error as this indicates a problem in the pipeline.
		return nil, fmt.Errorf("no Opus frames extracted from DCA conversion for '%s'", productNameForLog)
	}

	log.Printf("CONVERT_AUDIO: Successfully converted MP3 to %d DCA Opus frames for '%s'", len(soundFrames), productNameForLog)
	return soundFrames, nil
}

// --- End Google Cloud TTS Functions ---

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
			var soundToPlay [][]byte // Store dynamically generated sound here

			defer func() {
				playSoundMutex.Lock()
				isPlayingSound = false
				playSoundMutex.Unlock()
				if vc != nil {
					log.Printf("DCA_PLAY: Disconnecting voice for TCIN %s", alert.TCIN)
					vc.Disconnect() // Ensure disconnect
				}
				log.Println("DCA_PLAY: Goroutine finished for TCIN", alert.TCIN)
			}()

			// Synthesize speech and convert to DCA
			log.Printf("TTS_PIPELINE: Starting TTS for '%s' (TCIN: %s)", alert.Title, alert.TCIN)
			mp3Audio, errTTS := synthesizeSpeechGoogleCloud(alert.Title)
			if errTTS != nil {
				log.Printf("ERROR_TTS_PIPELINE: Failed to synthesize speech for '%s': %v", alert.Title, errTTS)
				// Fallback or error handling - for now, just log and skip playing.
				return
			}

			dcaFrames, errConvert := convertMP3ToDCAFrames(mp3Audio, alert.Title)
			if errConvert != nil {
				log.Printf("ERROR_TTS_PIPELINE: Failed to convert MP3 to DCA for '%s': %v", alert.Title, errConvert)
				return
			}
			if len(dcaFrames) == 0 {
				log.Printf("ERROR_TTS_PIPELINE: No DCA frames generated for '%s'. Skipping playback.", alert.Title)
				return
			}
			soundToPlay = dcaFrames
			log.Printf("TTS_PIPELINE: Successfully generated %d DCA frames for '%s'", len(soundToPlay), alert.Title)

			log.Printf("DCA_PLAY: Joining channel %s for TCIN %s (Product: '%s')", voiceChannelID, alert.TCIN, alert.Title)
			vc, err = discordSession.ChannelVoiceJoin(serverID, voiceChannelID, false, true) // Mute: false, Deaf: true
			if err != nil {
				log.Printf("ERROR_DCA_PLAY: Join error for TCIN %s ('%s'): %v", alert.TCIN, alert.Title, err)
				return
			}
			log.Printf("DCA_PLAY: Joined channel successfully for TCIN %s ('%s')", alert.TCIN, alert.Title)

			// Play the dynamically generated sound
			playSoundFromFrames(vc, soundToPlay, alert.TCIN, alert.Title)

		}(stockAlert)
	}
}

// playSoundFromFrames sends pre-loaded Opus frames to the voice connection.
// Updated to accept frames directly.
func playSoundFromFrames(vc *discordgo.VoiceConnection, framesToPlay [][]byte, tcinForLog string, titleForLog string) {
	log.Printf("playSoundFromFrames: Playing for TCIN %s ('%s'). Buffer size: %d frames.", tcinForLog, titleForLog, len(framesToPlay))
	if vc == nil {
		log.Printf("ERROR_DCA_PLAY: Voice connection is nil for TCIN %s ('%s').", tcinForLog, titleForLog)
		return
	}
	if len(framesToPlay) == 0 {
		log.Printf("ERROR_DCA_PLAY: No sound frames to play for TCIN %s ('%s').", tcinForLog, titleForLog)
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

	// time.Sleep(100 * time.Millisecond) // Short pause before speaking - can be adjusted or removed

	if err := vc.Speaking(true); err != nil {
		log.Printf("ERROR_DCA_PLAY: vc.Speaking(true) error for TCIN %s ('%s'): %v", tcinForLog, titleForLog, err)
		return
	}
	defer vc.Speaking(false)

	log.Printf("DCA_PLAY_DEBUG: Sending %d Opus frames for TCIN %s ('%s')...", len(framesToPlay), tcinForLog, titleForLog)
	for i, frame := range framesToPlay {
		if frame == nil || len(frame) == 0 {
			log.Printf("WARN_DCA_PLAY: Skipping nil or empty frame #%d for TCIN %s ('%s')", i+1, tcinForLog, titleForLog)
			continue
		}
		select {
		case vc.OpusSend <- frame:
			// log.Printf("DCA_PLAY_TRACE: Sent frame %d for TCIN %s", i+1, tcinForLog) // Very verbose
		case <-time.After(200 * time.Millisecond): // Timeout for sending a single frame (was 1 second, can be tuned)
			log.Printf("ERROR_DCA_PLAY: Timeout sending Opus frame #%d for TCIN %s ('%s'). Sent %d frames.", i+1, tcinForLog, titleForLog, i)
			return // Exit if send times out
		}
	}
	log.Printf("DCA_PLAY_DEBUG: Finished sending all %d frames for TCIN %s ('%s').", len(framesToPlay), tcinForLog, titleForLog)
	// time.Sleep(100 * time.Millisecond) // Brief pause after sending all frames - can be adjusted or removed
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
