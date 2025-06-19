package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	mrand "math/rand"
	standard_http "net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// --- Configuration Constants (values that are unlikely to change or are true constants) ---
const (
	// Updated to the new, more official Target logo PNG URL provided by the user.
	discordEmbedImageURL = "https://corporate.target.com/getmedia/0289d38f-1bb0-48f9-b883-cd05e19b8f98/Target_Bullseye-Logo_Red_transparent.png?width=1144"
	// Interval in seconds between checking all products
	checkIntervalSeconds        = 8.0  // Set to a higher value for production (e.g., 300 for 5 minutes)
	checkIntervalJitterSeconds  = 5    // Max seconds for random jitter
	proxyTestTimeoutSeconds     = 10   // Timeout for a single proxy test
	proxyMaxValidationLatencyMS = 5000 // Updated: Max latency in MS for a proxy (e.g., 500ms)
	notificationCooldownMinutes = 2    // Cooldown in minutes before resending a notification for the same TCIN
	quickRecheckIntervalSeconds = 5.0  // How often to run the quick re-check cycle
	quickRecheckCount           = 3    // How many quick re-checks to perform for an OOS item
	// New for multi-worker
	numMonitoringWorkers = 8 // Number of concurrent main monitoring workers

	// New for Product Thumbnail
	productThumbnailFormat = "png" // Desired format for product thumbnail
	productThumbnailWidth  = 150   // Desired width for product thumbnail
	productThumbnailHeight = 64    // Desired height for product thumbnail

	// ANSI Color Codes for logging
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	// colorBlue   = "\033[34m" // Example, add if needed
	productImagePrefetchTimeoutSeconds = 20 // Timeout for pre-fetching individual product page HTML

	// API Response Logging
	apiResponseLogFile       = "api_responses.log"
	enableApiResponseLogging = false // Set to false to disable

	// WebSocket Configuration
	websocketPort      = "6969"
	highStockThreshold = 10
	// Dedicated WebSocket Logging
	webSocketLogFile       = "websocket.log"
	enableWebSocketLogging = true // Set to false to disable WebSocket file logging
)

// --- Configuration Variables (to be loaded from .env or defaults) ---
var (
	discordWebhookURL string
	proxyFilePath     string
	proxyTestURL      string

	// List of TCINs to monitor - will be loaded from tcins.txt
	tcinsToMonitor []string

	// Store contexts to rotate through - UPDATED with user-provided data
	storeContexts = []StoreContext{
		{StoreID: "2364", ZipCode: "32136-3126", State: "FL"}, // Original Florida context
		{StoreID: "1016", ZipCode: "23834-3605", State: "VA"},
		{StoreID: "1017", ZipCode: "23831-5350", State: "VA"},
		{StoreID: "1292", ZipCode: "24073-1151", State: "VA"},
		// Removed other placeholder stores, user can add more if needed following this format
	}

	// Mobile API specific parameters (some are now dynamic or per-context)
	mobileAPIKey     = "3f015bca9bce7dbb2b377638fa5de0f229713c78"
	mobileAppVersion = "2025.21.0"
	// mobileStoreID, mobileZipCode, mobileState removed as they are now part of StoreContext
	// mobileVisitorID, mobileLoyaltyID, mobileMemberID are per-worker/per-call
	// mobileXRequestID is per-request

	// These might still be needed if re-introducing store context or for reference
	defaultMobileStoreID = "2364"
	// defaultMobileLoyaltyIDTemplate = "tly.01dc4ba6b1784977afbce47f34d83e5a"
	// defaultMobileMemberIDTemplate  = "10044685289"

	loadedProxies []string

	// Map to store the last notification time for each TCIN
	lastNotificationSent  map[string]time.Time
	lastNotificationMutex sync.Mutex

	// State tracking for advanced request chain
	lastKnownStockState      map[string]bool // TCIN -> true if in stock, false if OOS
	stateMutex               sync.Mutex
	quickRecheckCounters     map[string]int // TCIN -> number of quick re-checks remaining
	recheckMutex             sync.Mutex
	quickRecheckMobileClient tls_client.HttpClient                           // Dedicated client for quick re-checks
	ErrRateLimited           = errors.New("rate limited by API")             // Custom error for 429
	ErrNotFound              = errors.New("product not found via API (404)") // New error for 404

	// Cache for product image thumbnail URLs
	productImageCache      map[string]string // TCIN -> Formatted Product Thumbnail URL
	productImageCacheMutex sync.Mutex

	estLocation *time.Location // For EST timezone formatting

	// For API response logging
	apiResponseLogMutex sync.Mutex

	// For WebSocket logging
	webSocketLogMutex sync.Mutex

	// WebSocket server state
	wsClients      = make(map[*websocket.Conn]bool)
	wsClientsMutex sync.Mutex
	wsUpgrader     = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *standard_http.Request) bool { // Allow all origins for simplicity, tighten if needed
			return true
		},
	}

	// List of alternative TLS profiles for rotation, populated with available iOS/Safari profiles
	alternativeIOSProfiles = []utls.ClientHelloID{
		profiles.Safari_IOS_18_0.GetClientHelloId(), // Slightly older than current default
		profiles.Safari_IOS_17_0.GetClientHelloId(),
		profiles.Safari_IOS_16_0.GetClientHelloId(),
		profiles.Safari_IOS_15_6.GetClientHelloId(),
		profiles.Safari_IOS_15_5.GetClientHelloId(),
		profiles.Safari_16_0.GetClientHelloId(), // Generic Safari (desktop but often similar TLS)
		profiles.Safari_15_6_1.GetClientHelloId(),
		profiles.Safari_Ipad_15_6.GetClientHelloId(),
		// Consider adding these if they provide good diversity and are stable:
		// profiles.ZalandoIosMobile.GetClientHelloId(),
		// profiles.NikeIosMobile.GetClientHelloId(),
		// profiles.MMSIos.GetClientHelloId(),
		// profiles.MMSIos2.GetClientHelloId(), // If different from MMSIos
		// profiles.MMSIos3.GetClientHelloId(), // If different from MMSIos/MMSIos2
		// profiles.MeshIos.GetClientHelloId(),
		// profiles.MeshIos2.GetClientHelloId(), // If different
		// profiles.ConfirmedIos.GetClientHelloId(),
	}
	mainClientInitialProfile = profiles.Safari_IOS_18_5.GetClientHelloId() // Our starting profile

	// validIOSUserAgents list remains as populated in the previous step.
	// Ideally, this list would be mapped to the TLS profiles for perfect consistency.
	validIOSUserAgents = []string{
		fmt.Sprintf("Target/%s iPhone15,3 iOS/18.5 CFNetwork/3826.500.131 Darwin/24.5.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPhone15,2 iOS/18.4.1 CFNetwork/3820.200.112 Darwin/24.4.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPhone14,5 iOS/18.3 CFNetwork/3800.100.105 Darwin/24.3.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPhone14,4 iOS/18.2.1 CFNetwork/3780.50.90 Darwin/24.2.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPhone13,4 iOS/17.5.1 CFNetwork/3750.0.100 Darwin/23.5.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPhone13,2 iOS/17.4 CFNetwork/3740.10.80 Darwin/23.4.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPad13,10 iOS/18.5 CFNetwork/3826.500.131 Darwin/24.5.0", mobileAppVersion),
		fmt.Sprintf("Target/%s iPad13,16 iOS/18.4 CFNetwork/3820.200.112 Darwin/24.4.0", mobileAppVersion),
	}

	// No separate global desktopClient needed if each worker gets one, or if quickRecheck uses its own.
	quickRecheckDesktopClient tls_client.HttpClient // For quick recheck HTML scraping
)

// --- WebSocket Structures ---

// WebSocketMessage defines the structure for messages sent to WebSocket clients (e.g., the voice bot)
type WebSocketMessage struct {
	TCIN              string  `json:"tcin"`
	Title             string  `json:"title"`
	AvailableQuantity float64 `json:"availableQuantity"`
	ProductURL        string  `json:"productURL"`
}

// StoreContext holds StoreID, ZipCode, and State for API requests
type StoreContext struct {
	StoreID string
	ZipCode string
	State   string
}

// Helper function to get environment variables with a fallback default
func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		log.Printf("INFO: Loaded config for '%s' from environment.", key)
		return value
	}
	log.Printf("INFO: Config for '%s' not found in environment, using default: '%s'", key, fallback)
	return fallback
}

// --- Structs for Mobile Target API Response ---
type MobileResponseData struct {
	Data MobileProductData `json:"data"`
}
type MobileProductData struct {
	Product MobileProduct `json:"product"`
}
type MobileProduct struct {
	TCIN        string            `json:"tcin"`
	Item        MobileItem        `json:"item"`
	Fulfillment MobileFulfillment `json:"fulfillment"`
	Price       MobilePriceData   `json:"price"`
	Enrichment  MobileEnrichment  `json:"enrichment"`
}
type MobileItem struct {
	ProductDescription MobileProductDescription `json:"product_description"`
}
type MobileProductDescription struct {
	Title string `json:"title"`
}
type MobilePriceData struct {
	FormattedCurrentPrice string  `json:"formatted_current_price"`
	CurrentRetail         float64 `json:"current_retail"`
}
type MobileFulfillment struct {
	ShippingOptions MobileShippingOptions `json:"shipping_options"`
}
type MobileShippingOptions struct {
	AvailabilityStatus         string  `json:"availability_status"`
	AvailableToPromiseQuantity float64 `json:"available_to_promise_quantity"`
	LoyaltyAvailabilityStatus  string  `json:"loyalty_availability_status"`
	ReasonCode                 string  `json:"reason_code,omitempty"`
}
type MobileEnrichment struct {
	Images MobileImages `json:"images"`
}
type MobileImages struct {
	PrimaryImageURL string `json:"primary_image_url"`
}

// --- Structs for Desktop Target API Response ---
type DesktopResponseData struct {
	Data DesktopProductDataWrapper `json:"data"`
}
type DesktopProductDataWrapper struct {
	Product DesktopProductFulfillment `json:"product"`
}
type DesktopProductFulfillment struct {
	TCIN        string                 `json:"tcin"`
	Fulfillment DesktopFulfillmentData `json:"fulfillment"`
}
type DesktopFulfillmentData struct {
	ShippingOptions DesktopShippingOptions `json:"shipping_options"`
}
type DesktopShippingOptions struct {
	AvailabilityStatus string `json:"availability_status"`
}

// --- Structs for Discord Webhook ---
type DiscordMessage struct {
	Username  string         `json:"username,omitempty"`
	AvatarURL string         `json:"avatar_url,omitempty"`
	Content   string         `json:"content,omitempty"`
	Embeds    []DiscordEmbed `json:"embeds,omitempty"`
}

type DiscordEmbed struct {
	Author      EmbedAuthor    `json:"author,omitempty"`
	Title       string         `json:"title,omitempty"`
	URL         string         `json:"url,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"` // Hex color code (e.g., 0xFF0000 for red)
	Fields      []EmbedField   `json:"fields,omitempty"`
	Thumbnail   EmbedThumbnail `json:"thumbnail,omitempty"`
	Image       EmbedImage     `json:"image,omitempty"`
	Footer      EmbedFooter    `json:"footer,omitempty"`
}

type EmbedAuthor struct {
	Name    string `json:"name,omitempty"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type EmbedThumbnail struct {
	URL string `json:"url,omitempty"`
}

type EmbedImage struct {
	URL string `json:"url,omitempty"`
}

type EmbedFooter struct {
	Text    string `json:"text,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

// IPResponse struct to parse ipify.org's JSON response
type IPResponse struct {
	IP string `json:"ip"`
}

func shuffleProxies(proxies []string) {
	if len(proxies) > 0 {
		mrand.Shuffle(len(proxies), func(i, j int) {
			proxies[i], proxies[j] = proxies[j], proxies[i]
		})
	}
}

// loadProxies reads proxies from a file and formats them for tls-client
func loadProxies(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		// If the file doesn't exist, it's not a fatal error, just means no proxies will be used.
		if os.IsNotExist(err) {
			log.Printf("INFO: Proxy file '%s' not found. Continuing without proxies.", filePath)
			return nil, nil
		}
		return nil, fmt.Errorf("error opening proxy file %s: %w", filePath, err)
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") { // Skip empty lines and comments
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) == 2 { // host:port
			ip := parts[0]
			port := parts[1]
			proxies = append(proxies, fmt.Sprintf("http://%s:%s", ip, port))
		} else if len(parts) == 4 { // host:port:username:password
			ip := parts[0]
			port := parts[1]
			user := parts[2]
			pass := parts[3]
			proxies = append(proxies, fmt.Sprintf("http://%s:%s@%s:%s", user, pass, ip, port))
		} else {
			log.Printf("WARN: Invalid proxy format on line %d in %s: '%s' (expected host:port or host:port:username:password)", lineNumber, filePath, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading proxy file %s: %w", filePath, err)
	}

	if len(proxies) == 0 && lineNumber > 0 { // Check if file had lines but none were valid proxies
		log.Printf("INFO: No valid proxies found in '%s' after parsing %d lines. Continuing without proxies.", filePath, lineNumber)
	} else if len(proxies) == 0 { // File was empty or only comments
		log.Printf("INFO: Proxy file '%s' is empty or contains only comments. Continuing without proxies.", filePath)
	}
	if len(proxies) > 0 {
		log.Printf("Successfully loaded %d proxy candidates from '%s'.", len(proxies), filePath)
	}
	return proxies, nil
}

// validateProxies tests a list of proxy strings for connectivity and speed.
func validateProxies(initialProxies []string) []string {
	if len(initialProxies) == 0 {
		log.Println("INFO: No proxies provided to validate (list was empty).")
		return nil
	}

	log.Printf("INFO: Starting validation for %d loaded proxy candidates (Connectivity & Speed only)...", len(initialProxies))
	var wg sync.WaitGroup
	goodProxiesChan := make(chan string, len(initialProxies)) // Channel type is now string

	for _, proxyStr := range initialProxies {
		wg.Add(1)
		go func(pStr string) {
			defer wg.Done()

			// Test connectivity and speed via ipify.org
			tempClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
				tls_client.WithTimeoutSeconds(proxyTestTimeoutSeconds),
				tls_client.WithClientProfile(profiles.Chrome_133),
				tls_client.WithNotFollowRedirects(),
			)
			if err != nil {
				log.Printf("WARN: Proxy Test (Setup) - Failed to create temp client for proxy '%s': %v", pStr, err)
				return
			}
			if err := tempClient.SetProxy(pStr); err != nil {
				log.Printf("WARN: Proxy Test (Setup) - Failed to set proxy '%s' on temp client: %v", pStr, err)
				return
			}
			reqStartTime := time.Now()
			ipifyReq, err := http.NewRequest(http.MethodGet, proxyTestURL, nil)
			if err != nil {
				log.Printf("WARN: Proxy Test (ipify) - Failed to create request for '%s' via proxy '%s': %v", proxyTestURL, pStr, err)
				return
			}
			ipifyReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
			ipifyResp, err := tempClient.Do(ipifyReq)
			ipifyRequestDuration := time.Since(reqStartTime)
			if err != nil {
				log.Printf("WARN: Proxy Test (ipify) - Request via proxy '%s' to '%s' failed in %s: %v", pStr, proxyTestURL, ipifyRequestDuration, err)
				return
			}
			defer ipifyResp.Body.Close()
			if ipifyResp.StatusCode != standard_http.StatusOK {
				log.Printf("WARN: Proxy Test (ipify) - Proxy '%s' returned non-OK status (%d) from '%s' (latency: %s)", pStr, ipifyResp.StatusCode, proxyTestURL, ipifyRequestDuration)
				return
			}
			ipifyBody, errRead := io.ReadAll(ipifyResp.Body)
			if errRead != nil {
				log.Printf("WARN: Proxy Test (ipify) - Failed to read response body from '%s' via proxy '%s' (latency: %s): %v", proxyTestURL, pStr, ipifyRequestDuration, errRead)
				return
			}
			var ipInfo IPResponse
			if errJson := json.Unmarshal(ipifyBody, &ipInfo); errJson != nil {
				log.Printf("WARN: Proxy Test (ipify) - Failed to parse JSON IP response from '%s' via proxy '%s' (latency: %s, Body: '%s'): %v", proxyTestURL, pStr, ipifyRequestDuration, string(ipifyBody), errJson)
				return
			}
			if ipifyRequestDuration.Milliseconds() > proxyMaxValidationLatencyMS {
				log.Printf("INFO: Proxy Test (ipify) - Proxy '%s' is GOOD but SLOW. Latency: %s (External IP: %s). Discarding.", pStr, ipifyRequestDuration, ipInfo.IP)
				return
			}
			log.Printf("INFO: Proxy Test (ipify) - Proxy '%s' is GOOD and FAST. Latency: %s (External IP: %s). Keeping proxy.", pStr, ipifyRequestDuration, ipInfo.IP)

			// Scamalytics check removed.
			goodProxiesChan <- pStr // Send the proxy string directly

		}(proxyStr)
	}

	wg.Wait()
	close(goodProxiesChan)

	var finalGoodProxies []string
	for p := range goodProxiesChan {
		finalGoodProxies = append(finalGoodProxies, p)
	}

	if len(finalGoodProxies) == 0 && len(initialProxies) > 0 {
		log.Println("WARN: Proxy Test - No proxies passed connectivity/speed validation. Monitor will proceed without proxies.")
	} else if len(initialProxies) > 0 {
		log.Printf("INFO: Proxy Test - %d of %d initial proxies passed validation and will be used.", len(finalGoodProxies), len(initialProxies))
	}
	return finalGoodProxies
}

// preFetchProductImages fetches and caches product image URLs at startup.
func preFetchProductImages() {
	if len(tcinsToMonitor) == 0 {
		log.Println("INFO: No TCINs to monitor, skipping image pre-fetch.")
		return
	}
	log.Printf("INFO: Starting pre-fetch of product images for %d TCINs...", len(tcinsToMonitor))

	var wg sync.WaitGroup
	tcinsSnapshot := make([]string, len(tcinsToMonitor))
	copy(tcinsSnapshot, tcinsToMonitor)

	localProxiesForPrefetch := make([]string, len(loadedProxies))
	copiedProxies := false
	if len(loadedProxies) > 0 {
		copy(localProxiesForPrefetch, loadedProxies)
		shuffleProxies(localProxiesForPrefetch)
		copiedProxies = true
	}

	for i, tcin := range tcinsSnapshot {
		wg.Add(1)

		proxyToUseForThisPrefetch := ""
		if copiedProxies && len(localProxiesForPrefetch) > 0 {
			proxyToUseForThisPrefetch = localProxiesForPrefetch[i%len(localProxiesForPrefetch)]
		}

		go func(currentTCIN string, proxyToUse string) {
			defer wg.Done()
			var fetchedImageURL string // Default to empty

			// Create a new, short-lived client for each pre-fetch goroutine for proxy isolation
			preFetchClientJar := tls_client.NewCookieJar()
			preFetchClientOptions := []tls_client.HttpClientOption{
				tls_client.WithTimeoutSeconds(productImagePrefetchTimeoutSeconds),
				tls_client.WithClientProfile(profiles.Chrome_133),
				tls_client.WithNotFollowRedirects(),
				tls_client.WithCookieJar(preFetchClientJar),
			}
			preFetchClient, clientErr := tls_client.NewHttpClient(tls_client.NewNoopLogger(), preFetchClientOptions...)
			if clientErr != nil {
				log.Printf("WARN: Pre-fetch TCIN %s - Failed to create temp pre-fetch client: %v", currentTCIN, clientErr)
				productImageCacheMutex.Lock()
				productImageCache[currentTCIN] = "" // Cache failure
				productImageCacheMutex.Unlock()
				return
			}

			if proxyToUse != "" {
				if err := preFetchClient.SetProxy(proxyToUse); err != nil {
					log.Printf("WARN: Pre-fetch TCIN %s - Failed to set proxy %s: %v. Proceeding without proxy for this attempt.", currentTCIN, proxyToUse, err)
					// If proxy setting fails, preFetchClient will make a direct request.
				}
			}

			productPageURL := fmt.Sprintf("https://www.target.com/p/-/A-%s", currentTCIN)
			pageReq, pageErr := http.NewRequest("GET", productPageURL, nil)
			if pageErr == nil {
				pageReq.Header = http.Header{
					"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
					"Accept-Language":           {"en-US,en;q=0.9"},
					"Cache-Control":             {"no-cache"},
					"Pragma":                    {"no-cache"},
					"Priority":                  {"u=0, i"},
					"Sec-Fetch-Dest":            {"document"},
					"Sec-Fetch-Mode":            {"navigate"},
					"Sec-Fetch-Site":            {"none"},
					"Sec-Fetch-User":            {"?1"},
					"Sec-GPC":                   {"1"},
					"Upgrade-Insecure-Requests": {"1"},
					"User-Agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"},
				}
				pageResp, pageRespErr := preFetchClient.Do(pageReq)
				if pageRespErr == nil {
					defer pageResp.Body.Close()
					if pageResp.StatusCode == standard_http.StatusOK {
						htmlBody, readErr := io.ReadAll(pageResp.Body)
						if readErr == nil {
							re := regexp.MustCompile(`https://target\.scene7\.com/is/image/Target/[A-Za-z0-9_-]+`)
							foundBaseImageURL := re.FindString(string(htmlBody))
							if foundBaseImageURL != "" {
								fetchedImageURL = fmt.Sprintf("%s?format=%s&width=%d&height=%d",
									foundBaseImageURL, productThumbnailFormat, productThumbnailWidth, productThumbnailHeight)
								log.Printf("INFO: Pre-fetch TCIN %s - Found image URL: %s", currentTCIN, fetchedImageURL)
							} else {
								log.Printf("WARN: Pre-fetch TCIN %s - Product image URL pattern not found in HTML from %s.", currentTCIN, productPageURL)
							}
						} else {
							log.Printf("WARN: Pre-fetch TCIN %s - Failed to read page body from %s: %v", currentTCIN, productPageURL, readErr)
						}
					} else {
						log.Printf("WARN: Pre-fetch TCIN %s - Page request to %s returned status %d", currentTCIN, productPageURL, pageResp.StatusCode)
					}
				} else {
					log.Printf("WARN: Pre-fetch TCIN %s - Failed to fetch page %s: %v", currentTCIN, productPageURL, pageRespErr)
				}
			} else {
				log.Printf("WARN: Pre-fetch TCIN %s - Failed to create request for page %s: %v", currentTCIN, productPageURL, pageErr)
			}

			productImageCacheMutex.Lock()
			productImageCache[currentTCIN] = fetchedImageURL
			productImageCacheMutex.Unlock()
		}(tcin, proxyToUseForThisPrefetch)
	}
	wg.Wait()
	log.Println("INFO: Product image pre-fetching attempts complete.")
}

func main() {
	mrand.Seed(time.Now().UnixNano())
	lastNotificationSent = make(map[string]time.Time)
	lastKnownStockState = make(map[string]bool)
	quickRecheckCounters = make(map[string]int)
	productImageCache = make(map[string]string)

	errEnv := godotenv.Load()
	if errEnv != nil {
		if !os.IsNotExist(errEnv) {
			log.Printf("WARN: Error loading .env file: %v. Will use defaults or system env vars.", errEnv)
		} else {
			log.Println("INFO: .env file not found. Using defaults or system environment variables for configuration.")
		}
	}

	// Load TCINs from file
	var errLoadTcins error
	tcinsToMonitor, errLoadTcins = loadTcinsFromFile("tcins.txt")
	if errLoadTcins != nil {
		log.Printf("ERROR: Critical error loading TCINs from 'tcins.txt': %v. Monitor will proceed without TCINs.", errLoadTcins)
		tcinsToMonitor = []string{} // Ensure it's an empty slice on error
	}
	if len(tcinsToMonitor) == 0 {
		log.Println("WARN: No TCINs loaded to monitor. The application will run but will not check any products.")
	}

	// Start WebSocket server
	// Use standard_http for the WebSocket server listener and handler
	standard_http.HandleFunc("/ws", handleWebSocketConnections)
	go func() {
		logMsg := fmt.Sprintf("WebSocket server starting on port %s", websocketPort)
		log.Println("INFO: " + logMsg) // Keep console log for startup
		logToWebSocketFile(logMsg)     // Log to dedicated file

		if err := standard_http.ListenAndServe(":"+websocketPort, nil); err != nil {
			fatalMsg := fmt.Sprintf("WebSocket server ListenAndServe error: %v", err)
			logToWebSocketFile("FATAL: " + fatalMsg) // Attempt to log to file before fatal exit
			log.Fatalf("FATAL: " + fatalMsg)         // Console log and exit
		}
	}()

	discordWebhookURL = getEnv("DISCORD_WEBHOOK_URL", "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE")
	proxyFilePath = getEnv("PROXY_FILE_PATH", "proxies.txt")
	proxyTestURL = getEnv("PROXY_TEST_URL", "https://api.ipify.org?format=json")

	if discordWebhookURL == "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE" || discordWebhookURL == "" {
		log.Println("WARN: DISCORD_WEBHOOK_URL is not set or is using the default placeholder. Notifications will likely fail.")
	}

	var err error
	estLocation, err = time.LoadLocation("America/New_York")
	if err != nil {
		log.Printf("WARN: Could not load America/New_York timezone: %v. Footer timestamps will use system's local time.", err)
		estLocation = time.Local
	}

	log.Println("Target Product Monitor started.")

	candidates, errLoadProxies := loadProxies(proxyFilePath)
	if errLoadProxies != nil {
		log.Printf("ERROR: Critical error loading proxies from '%s': %v. Monitor will proceed without proxies.", proxyFilePath, errLoadProxies)
		loadedProxies = nil
	} else {
		if len(candidates) > 0 {
			loadedProxies = validateProxies(candidates)
			if len(loadedProxies) == 0 {
				log.Println("INFO: No proxies were validated successfully. Monitor will proceed without proxies.")
			}
		} else {
			loadedProxies = nil
		}
	}

	// Initialize Quick Re-check Clients (as before)
	quickRecheckJarMobile := tls_client.NewCookieJar()
	quickRecheckOptionsMobile := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(15),
		tls_client.WithClientProfile(profiles.Safari_IOS_18_5),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(quickRecheckJarMobile),
	}
	quickRecheckMobileClient, err = tls_client.NewHttpClient(tls_client.NewNoopLogger(), quickRecheckOptionsMobile...)
	if err != nil {
		log.Fatalf("Failed to create Quick Re-check Mobile TLS client: %v", err)
	}
	quickRecheckJarDesktop := tls_client.NewCookieJar()
	quickRecheckOptionsDesktop := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(20),
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(quickRecheckJarDesktop),
	}
	quickRecheckDesktopClient, err = tls_client.NewHttpClient(tls_client.NewNoopLogger(), quickRecheckOptionsDesktop...)
	if err != nil {
		log.Fatalf("Failed to create Quick Re-check Desktop TLS client: %v", err)
	}
	log.Println("Quick Re-check clients initialized.")

	// --- Pre-fetch Product Images (Synchronously from main goroutine's perspective) ---
	if len(tcinsToMonitor) > 0 {
		log.Println("MAIN: Starting product image pre-fetching...")
		preFetchProductImages() // Call directly. This function uses its own WaitGroup for internal concurrency.
		log.Println("MAIN: Product image pre-fetching complete. Starting monitoring workers.")
	} else {
		log.Println("MAIN: No TCINs to monitor, skipping image pre-fetch.")
	}
	// --- End Pre-fetch ---

	go quickRecheckWorker() // Start the quick re-check worker (can start after image prefetch or concurrently, it has its own dependencies)

	log.Printf("Initializing %d main monitoring workers...", numMonitoringWorkers)
	staggerDelay := time.Duration(0)
	if numMonitoringWorkers > 0 && len(storeContexts) > 0 {
		staggerDelay = (time.Duration(checkIntervalSeconds) * time.Second) / time.Duration(numMonitoringWorkers)
	} else if numMonitoringWorkers > 0 {
		staggerDelay = (time.Duration(checkIntervalSeconds) * time.Second) / time.Duration(numMonitoringWorkers)
	}

	for workerID := 0; workerID < numMonitoringWorkers; workerID++ {
		// Construct the initial profiles.ClientProfile for this worker
		initialProfileForWorker := profiles.NewClientProfile(
			mainClientInitialProfile, // This is utls.ClientHelloID
			profiles.DefaultClientProfile.GetSettings(),
			profiles.DefaultClientProfile.GetSettingsOrder(),
			profiles.DefaultClientProfile.GetPseudoHeaderOrder(),
			profiles.DefaultClientProfile.GetConnectionFlow(),
			nil,
			nil,
		)

		workerMobileJar := tls_client.NewCookieJar()
		workerMobileOptions := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithClientProfile(initialProfileForWorker), // Use the constructed profiles.ClientProfile
			tls_client.WithNotFollowRedirects(),
			tls_client.WithCookieJar(workerMobileJar),
		}
		workerMobileAPIClient, workerErr := tls_client.NewHttpClient(tls_client.NewNoopLogger(), workerMobileOptions...)
		if workerErr != nil {
			log.Fatalf("WORKER %d: Failed to create Main Mobile API TLS client: %v", workerID, workerErr)
		}
		log.Printf("WORKER %d: Main Mobile API client initialized with profile based on HelloID: %s", workerID, mainClientInitialProfile.Str())

		workerDesktopJar := tls_client.NewCookieJar()
		workerDesktopOptions := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(productImagePrefetchTimeoutSeconds),
			tls_client.WithClientProfile(profiles.Chrome_133),
			tls_client.WithNotFollowRedirects(),
			tls_client.WithCookieJar(workerDesktopJar),
		}
		workerHTMLClient, workerErrDesktop := tls_client.NewHttpClient(tls_client.NewNoopLogger(), workerDesktopOptions...)
		if workerErrDesktop != nil {
			log.Fatalf("WORKER %d: Failed to create Desktop HTML TLS client: %v", workerID, workerErrDesktop)
		}
		log.Printf("WORKER %d: Desktop HTML client initialized.", workerID)

		initialStoreContextIndex := uint64(0)
		if len(storeContexts) > 0 {
			initialStoreContextIndex = uint64(workerID % len(storeContexts))
		}
		go monitoringWorker(workerID, workerMobileAPIClient, workerHTMLClient, initialStoreContextIndex)
		if workerID < numMonitoringWorkers-1 && staggerDelay > 0 {
			time.Sleep(staggerDelay)
		}
	}

	log.Println("All monitoring workers launched. Main goroutine will now block.")
	select {}
}

func monitoringWorker(workerID int, mobileAPIClient tls_client.HttpClient, htmlScrapeClient tls_client.HttpClient, initialStoreContextIdx uint64) {
	log.Printf("WORKER %d: Started.", workerID)
	client := mobileAPIClient // client for API calls
	var currentWorkerProxyIndex uint64
	var currentBackoffDuration time.Duration
	var consecutiveBadCycles int
	currentProfileIndex := -1                      // -1 means using the initial profile, 0 and up for alternatives
	currentUsedHelloID := mainClientInitialProfile // Track the HelloID currently in use by this worker's client
	currentStoreCtxIndex := initialStoreContextIdx

	// Worker-specific, semi-persistent identifiers
	workerVisitorID, err := generateRandomHexString(32)
	if err != nil {
		log.Printf("ERROR: WORKER %d: Failed to generate VisitorID: %v. Using a default.", workerID, err)
		workerVisitorID = "01010101010101010101010101010101" // Fallback
	}
	randomHexForLoyalty, _ := generateRandomHexString(32)
	workerLoyaltyID := "tly." + randomHexForLoyalty
	workerMemberID := generateRandomNumericString(10) // e.g., 10-digit member ID
	workerUserAgent := validIOSUserAgents[mrand.Intn(len(validIOSUserAgents))]
	log.Printf("WORKER %d: Using VisitorID: %s, LoyaltyID: %s, MemberID: %s, User-Agent: %s",
		workerID, workerVisitorID, workerLoyaltyID, workerMemberID, workerUserAgent)

	for {
		if currentBackoffDuration > 0 {
			log.Printf("WORKER %d: Currently in backoff mode. Waiting for %s before next cycle.", workerID, currentBackoffDuration)
			time.Sleep(currentBackoffDuration)
			currentBackoffDuration = 0
			log.Printf("WORKER %d: Backoff complete. Resuming normal cycle.", workerID)
		}

		workerProxies := make([]string, len(loadedProxies))
		copied := false
		if len(loadedProxies) > 0 {
			copy(workerProxies, loadedProxies)
			shuffleProxies(workerProxies)
			copied = true
		}

		var workerCycleWg sync.WaitGroup
		var cycleHadErrorsRequiringAction bool // Broader flag for 404 or 429

		for i, tcin := range tcinsToMonitor {
			workerCycleWg.Add(1)
			proxyToUse := ""
			if copied && len(workerProxies) > 0 {
				proxyIndex := (currentWorkerProxyIndex + uint64(i)) % uint64(len(workerProxies))
				proxyToUse = workerProxies[proxyIndex]
			}

			go func(currentTCIN string, currentProxy string, sc StoreContext) {
				defer workerCycleWg.Done()
				if currentProxy != "" {
					if err := client.SetProxy(currentProxy); err != nil { // Set proxy on mobileAPIClient
						log.Printf("WARN: WORKER %d - TCIN %s: Failed to set proxy %s for Mobile API Client: %v.", workerID, currentTCIN, currentProxy, err)
					}
					// Also set proxy for the HTML scrape client for this TCIN check
					if err := htmlScrapeClient.SetProxy(currentProxy); err != nil {
						log.Printf("WARN: WORKER %d - TCIN %s: Failed to set proxy %s for HTML Scrape Client: %v.", workerID, currentTCIN, currentProxy, err)
					}
				}
				// Pass htmlScrapeClient to checkProductMobileAPIAndNotify
				err := checkProductMobileAPIAndNotify(client, htmlScrapeClient, currentTCIN, false,
					workerVisitorID, workerUserAgent, workerLoyaltyID, workerMemberID,
					sc.StoreID, sc.ZipCode, sc.State)
				if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrNotFound) {
					cycleHadErrorsRequiringAction = true
				}
			}(tcin, proxyToUse, storeContexts[currentStoreCtxIndex%uint64(len(storeContexts))])
		}

		if copied && len(workerProxies) > 0 {
			currentWorkerProxyIndex = (currentWorkerProxyIndex + uint64(len(tcinsToMonitor))) % uint64(len(workerProxies))
		}
		workerCycleWg.Wait()

		if cycleHadErrorsRequiringAction {
			currentBackoffDuration = 1 * time.Minute // Shorter initial backoff, e.g. 1 min
			consecutiveBadCycles++
			log.Printf("WARN: WORKER %d: Errors (404/429) detected in cycle. Activating backoff for %s. Consecutive bad cycles: %d", workerID, currentBackoffDuration, consecutiveBadCycles)

			if consecutiveBadCycles >= 2 && len(alternativeIOSProfiles) > 0 {
				currentProfileIndex = (currentProfileIndex + 1) % len(alternativeIOSProfiles)
				newHelloID := alternativeIOSProfiles[currentProfileIndex]

				// Avoid rotating to the same profile currently in use if possible (only if multiple alternatives exist)
				if newHelloID.Str() == currentUsedHelloID.Str() && len(alternativeIOSProfiles) > 1 {
					currentProfileIndex = (currentProfileIndex + 1) % len(alternativeIOSProfiles)
					newHelloID = alternativeIOSProfiles[currentProfileIndex]
				}

				if newHelloID.Str() != currentUsedHelloID.Str() { // Only attempt rotation if the new profile is different
					log.Printf("INFO: WORKER %d: Attempting TLS fingerprint rotation to profile based on HelloID: %s due to %d consecutive bad cycles.", workerID, newHelloID.Str(), consecutiveBadCycles)

					newClientJar := tls_client.NewCookieJar()
					if cj, ok := client.GetCookieJar().(tls_client.CookieJar); ok && cj != nil {
						newClientJar = cj
					}

					// Use default settings from a known profile, as these are generally standard for HTTP/2
					newClientProfile := profiles.NewClientProfile(
						newHelloID,
						profiles.DefaultClientProfile.GetSettings(),
						profiles.DefaultClientProfile.GetSettingsOrder(),
						profiles.DefaultClientProfile.GetPseudoHeaderOrder(),
						profiles.DefaultClientProfile.GetConnectionFlow(),
						nil, // No custom http2.PriorityFrames
						nil, // No custom http2.HeaderPriorityParam
					)

					newClientOptions := []tls_client.HttpClientOption{
						tls_client.WithTimeoutSeconds(30),
						tls_client.WithClientProfile(newClientProfile),
						tls_client.WithNotFollowRedirects(),
						tls_client.WithCookieJar(newClientJar),
					}
					newWorkerClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), newClientOptions...)
					if err != nil {
						log.Printf("ERROR: WORKER %d: Failed to create new TLS client with rotated profile based on HelloID %s: %v. Continuing with old client.", workerID, newHelloID.Str(), err)
					} else {
						client = newWorkerClient
						currentUsedHelloID = newHelloID // Update the currently used profile ID
						log.Printf("INFO: WORKER %d: Successfully rotated TLS fingerprint (using HelloID: %s).", workerID, newHelloID.Str())
						consecutiveBadCycles = 0
						currentBackoffDuration = 0
					}
				} else {
					log.Printf("INFO: WORKER %d: New profile for rotation is same as current or no alternatives. Skipping rotation this time.", workerID)
				}
			}
		} else {
			currentBackoffDuration = 0
			consecutiveBadCycles = 0 // Reset if cycle was good
		}

		log.Printf("WORKER %d: All TCINs checked in this cycle.", workerID)

		baseInterval := time.Duration(checkIntervalSeconds) * time.Second
		jitter := time.Duration(mrand.Intn(checkIntervalJitterSeconds+1)) * time.Second
		actualSleepDuration := baseInterval + jitter
		log.Printf("WORKER %d: --- Waiting for %s (base: %.0fs, jitter: %s) before next check cycle ---", workerID, actualSleepDuration, checkIntervalSeconds, jitter)
		time.Sleep(actualSleepDuration)

		// Advance store context for the next cycle for this worker
		currentStoreCtxIndex = (currentStoreCtxIndex + 1) % uint64(len(storeContexts))
	}
}

func quickRecheckWorker() {
	log.Println("QUICK RE-CHECK WORKER: Started.")
	for {
		time.Sleep(time.Duration(quickRecheckIntervalSeconds) * time.Second)
		var tcinsToActuallyRecheckThisIteration []string
		recheckMutex.Lock()
		if len(quickRecheckCounters) == 0 {
			recheckMutex.Unlock()
			continue
		}
		for tcin, count := range quickRecheckCounters {
			if count > 0 {
				tcinsToActuallyRecheckThisIteration = append(tcinsToActuallyRecheckThisIteration, tcin)
				quickRecheckCounters[tcin] = count - 1
				if quickRecheckCounters[tcin] == 0 {
					delete(quickRecheckCounters, tcin)
				}
			}
		}
		recheckMutex.Unlock()

		if len(tcinsToActuallyRecheckThisIteration) > 0 {
			workerQuickRecheckProxies := make([]string, len(loadedProxies))
			copied := false
			if len(loadedProxies) > 0 {
				copy(workerQuickRecheckProxies, loadedProxies)
				shuffleProxies(workerQuickRecheckProxies)
				copied = true
			}

			// Quick re-checks can pick a store context, e.g., randomly or cycle like main workers
			// For simplicity, let's use a random store context for each batch of quick re-checks
			var selectedStoreContext StoreContext
			if len(storeContexts) > 0 {
				selectedStoreContext = storeContexts[mrand.Intn(len(storeContexts))]
			} else {
				// Fallback to default if no store contexts - this requires defaultStoreID etc. to be defined
				selectedStoreContext = StoreContext{StoreID: defaultMobileStoreID, ZipCode: "00000", State: "XX"} // Placeholder
			}

			var quickCheckWg sync.WaitGroup
			for i, tcinToRecheck := range tcinsToActuallyRecheckThisIteration {
				quickCheckWg.Add(1)
				proxyToUse := ""
				if copied && len(workerQuickRecheckProxies) > 0 {
					proxyIndex := i % len(workerQuickRecheckProxies)
					proxyToUse = workerQuickRecheckProxies[proxyIndex]
				}
				go func(currentTCIN string, currentProxy string, sc StoreContext) {
					defer quickCheckWg.Done()
					if currentProxy != "" {
						if err := quickRecheckMobileClient.SetProxy(currentProxy); err != nil {
							log.Printf("WARN: QUICK RE-CHECK - TCIN %s: Failed to set proxy %s for Mobile Client: %v.", currentTCIN, currentProxy, err)
						}
						// Also set for the desktop client used by quick rechecks for HTML scraping
						if err := quickRecheckDesktopClient.SetProxy(currentProxy); err != nil {
							log.Printf("WARN: QUICK RE-CHECK - TCIN %s: Failed to set proxy %s for Desktop HTML Client: %v.", currentTCIN, currentProxy, err)
						}
					}
					quickVisitorID, _ := generateRandomHexString(32)
					quickUserAgent := validIOSUserAgents[mrand.Intn(len(validIOSUserAgents))]
					randomHexForLoyalty, _ := generateRandomHexString(32)
					quickLoyaltyID := "tly." + randomHexForLoyalty
					quickMemberID := generateRandomNumericString(10)
					// Pass quickRecheckDesktopClient for HTML scraping
					err := checkProductMobileAPIAndNotify(quickRecheckMobileClient, quickRecheckDesktopClient, currentTCIN, true,
						quickVisitorID, quickUserAgent, quickLoyaltyID, quickMemberID,
						sc.StoreID, sc.ZipCode, sc.State)
					if errors.Is(err, ErrRateLimited) {
						log.Printf("WARN: QUICK RE-CHECK - TCIN %s (via %s): Received rate limit error (429).", currentTCIN, quickRecheckMobileClient.GetProxy())
					} else if errors.Is(err, ErrNotFound) {
						log.Printf("WARN: QUICK RE-CHECK - TCIN %s (via %s): Received Not Found (404).", currentTCIN, quickRecheckMobileClient.GetProxy())
						// Optionally, stop quick re-checks for this TCIN if it gets consistent 404s even in quick re-checks
						// recheckMutex.Lock()
						// delete(quickRecheckCounters, currentTCIN)
						// recheckMutex.Unlock()
					} else if err != nil {
						log.Printf("ERROR: QUICK RE-CHECK - TCIN %s (via %s): Error during check: %v", currentTCIN, quickRecheckMobileClient.GetProxy(), err)
					}
				}(tcinToRecheck, proxyToUse, selectedStoreContext)
			}
			quickCheckWg.Wait()
		}
	}
}

// Helper function to generate W3C Traceparent string
func generateTraceparentString() (string, error) {
	traceIDBytes := make([]byte, 16)
	if _, err := rand.Read(traceIDBytes); err != nil {
		return "", fmt.Errorf("failed to generate traceID for traceparent: %w", err)
	}
	spanIDBytes := make([]byte, 8)
	if _, err := rand.Read(spanIDBytes); err != nil {
		return "", fmt.Errorf("failed to generate spanID for traceparent: %w", err)
	}
	// Format: 00-hex(traceID)-hex(spanID)-01 (sampled)
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceIDBytes), hex.EncodeToString(spanIDBytes)), nil
}

// Helper function to generate a random hex string (for visitor ID)
func generateRandomHexString(length int) (string, error) {
	b := make([]byte, length/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Helper function to generate a random numeric string of specified length
func generateRandomNumericString(length int) string {
	bytes := make([]byte, length)
	for i := range bytes {
		bytes[i] = byte(mrand.Intn(10) + '0') // Generate a digit '0'-'9'
	}
	return string(bytes)
}

// checkProductMobileAPIAndNotify is the core function for checking a single product.
func checkProductMobileAPIAndNotify(apiClient tls_client.HttpClient, htmlScrapeClient tls_client.HttpClient,
	tcin string, isQuickRecheck bool, visitorID string, userAgent string, loyaltyID string, memberID string,
	storeID string, zipCode string, state string) error {

	//startTime := time.Now()

	pageParam := fmt.Sprintf("/pdplite/A-%s", tcin)
	requestURL := fmt.Sprintf("https://redsky.target.com/redsky_aggregations/v1/apps/pdp_lite_v1?channel=APPS&key=%s&pricing_store_id=%s&scheduled_delivery_store_id=%s&state=%s&store_id=%s&tcin=%s&visitor_id=%s&zip=%s&os_family=iOS&page=%s&app_version=%s",
		url.QueryEscape(mobileAPIKey),
		url.QueryEscape(storeID),
		url.QueryEscape(storeID),
		url.QueryEscape(state),
		url.QueryEscape(storeID),
		url.QueryEscape(tcin),
		url.QueryEscape(visitorID),
		url.QueryEscape(zipCode),
		url.QueryEscape(pageParam),
		url.QueryEscape(mobileAppVersion),
	)

	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		log.Printf("TCIN %s: Failed to create API request: %v", tcin, err)
		return err
	}
	newDeviceID := uuid.NewString()
	newTraceparent, _ := generateTraceparentString()
	newXRequestID := uuid.NewString()
	req.Header = http.Header{
		"X-VISITOR-ID":      {visitorID},
		"Accept":            {"application/json, text/plain, */*"},
		"x-scr":             {"42d16a82"},
		"X-CLIENT-VERSION":  {mobileAppVersion},
		"Accept-Encoding":   {"gzip, deflate, br"},
		"Accept-Language":   {"en-US,en;q=0.9"},
		"X-CLIENT-PLATFORM": {"iPhone"},
		"X-CHANNEL-ID":      {"APPS"},
		"x-sapphire-context": {fmt.Sprintf("app_name=Target&app_version=%s&base_membership=true&card_membership=true&channel=apps&device=iPhone15,3&in_store=false&loyalty_id=%s&member_id=%s&os_family=iOS&os_version=18.5&paid_membership=false&profile_created_date=2022-02-21T20:35:57.859Z&redcard_holder=true&source=flagship_ios&store_id=%s&tm=false&visitor_id=%s&wholeAppTest=true",
			mobileAppVersion, loyaltyID, memberID, storeID, visitorID)},
		"X-REQUEST-ID":   {newXRequestID},
		"User-Agent":     {userAgent},
		"X-DEVICE-ID":    {newDeviceID},
		"X-DEVICE-MODEL": {"iPhone15,3"},
		"traceparent":    {newTraceparent},
		http.HeaderOrderKey: {
			"X-VISITOR-ID", "Accept", "x-scr", "X-CLIENT-VERSION", "Accept-Encoding", "Accept-Language",
			"X-CLIENT-PLATFORM", "X-CHANNEL-ID", "x-sapphire-context", "X-REQUEST-ID", "User-Agent",
			"X-DEVICE-ID", "X-DEVICE-MODEL", "traceparent",
		},
	}

	resp, err := apiClient.Do(req)
	if err != nil {
		log.Printf("TCIN %s: Failed to execute API request: %v", tcin, err)
		return err
	}
	defer resp.Body.Close()

	//duration := time.Since(startTime)
	//log.Printf("TCIN %s: API Request completed in %s. Status: %s (%d)", tcin, duration, resp.Status, resp.StatusCode)

	body, errReadBody := io.ReadAll(resp.Body)
	if errReadBody != nil {
		log.Printf("TCIN %s: Failed to read API response body: %v", tcin, errReadBody)
		return errReadBody
	}

	// Log the raw API response to a file if enabled
	if enableApiResponseLogging {
		proxyUsed := apiClient.GetProxy()
		if proxyUsed == "" {
			proxyUsed = "DIRECT"
		}
		logMessage := fmt.Sprintf("[%s] TCIN: %s | Status: %d | Proxy: %s | Body: %s\n---\n",
			time.Now().Format(time.RFC3339Nano), tcin, resp.StatusCode, proxyUsed, string(body))
		go logToFile(logMessage) // Log concurrently to avoid blocking
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		log.Printf("WARN: TCIN %s: RATE LIMITED BY TARGET API (HTTP 429). Body: %s", tcin, string(body))
		return ErrRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		log.Printf("WARN: TCIN %s: Product/Endpoint NOT FOUND (HTTP 404) via API. Body: %s", tcin, string(body))
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("TCIN %s: Non-OK HTTP status from API: %d. Body: %s", tcin, resp.StatusCode, string(body))
		return fmt.Errorf("non-OK HTTP status from API: %d for TCIN %s", resp.StatusCode, tcin)
	}

	var responseData MobileResponseData
	if err := json.Unmarshal(body, &responseData); err != nil {
		log.Printf("TCIN %s: Failed to unmarshal API JSON: %v. Body: %s", tcin, err, string(body))
		return err
	}
	product := responseData.Data.Product
	if product.TCIN == "" {
		log.Printf("TCIN %s: Product TCIN not found in API response. Raw response: %s", tcin, string(body))
		return fmt.Errorf("product TCIN not found in API response for TCIN %s", tcin)
	}

	cleanedTitle := html.UnescapeString(product.Item.ProductDescription.Title)
	shippingStatus := product.Fulfillment.ShippingOptions.AvailabilityStatus
	availableQty := product.Fulfillment.ShippingOptions.AvailableToPromiseQuantity

	productImageCacheMutex.Lock()
	cachedProductImageURL, foundInCache := productImageCache[tcin]
	productImageCacheMutex.Unlock()

	var finalProductThumbnailURL string
	if !foundInCache {
		log.Printf("INFO: TCIN %s - Image not yet in cache (pre-fetch may be pending or TCIN is new/failed pre-fetch). No product thumbnail for this check.", tcin)
		// finalProductThumbnailURL will be its zero value (empty string), which is fine.
	} else {
		finalProductThumbnailURL = cachedProductImageURL
	}

	// New stock determination logic based on ReasonCode
	// An item is considered IN STOCK if ReasonCode is NOT "INVENTORY_UNAVAILABLE".
	// This includes cases where ReasonCode is empty/absent or has a different value.
	isInStock := (product.Fulfillment.ShippingOptions.ReasonCode != "INVENTORY_UNAVAILABLE")

	coloredShippingStatus := shippingStatus // Default to original status string for logging
	var stockReasoning string

	if isInStock {
		coloredShippingStatus = colorGreen + shippingStatus + colorReset
		if product.Fulfillment.ShippingOptions.ReasonCode != "" {
			stockReasoning = fmt.Sprintf("Considered IN STOCK (ReasonCode: '%s', Original Status: '%s')", product.Fulfillment.ShippingOptions.ReasonCode, shippingStatus)
		} else {
			stockReasoning = fmt.Sprintf("Considered IN STOCK (ReasonCode: [absent/empty], Original Status: '%s')", shippingStatus)
		}
	} else { // Not in stock (ReasonCode IS "INVENTORY_UNAVAILABLE")
		coloredShippingStatus = colorRed + shippingStatus + colorReset
		stockReasoning = fmt.Sprintf("Considered OUT OF STOCK (ReasonCode: '%s', Original Status: '%s')", product.Fulfillment.ShippingOptions.ReasonCode, shippingStatus)
	}
	log.Printf("INFO: TCIN %s - %s", tcin, stockReasoning) // Log the reasoning

	// Log the main status line - coloredShippingStatus reflects the ReasonCode logic
	log.Printf("TCIN %s: '%s' - Price: %s, Display Status: %s, Qty: %.0f (API Status: '%s', ReasonCode: '%s')",
		product.TCIN, cleanedTitle, product.Price.FormattedCurrentPrice, coloredShippingStatus, availableQty, shippingStatus, product.Fulfillment.ShippingOptions.ReasonCode)

	stateMutex.Lock()
	previousState, stateKnown := lastKnownStockState[tcin]
	lastKnownStockState[tcin] = isInStock // Update state based on isInStock
	stateMutex.Unlock()

	if isInStock { // This now correctly reflects only if status was truly "IN_STOCK"
		lastNotificationMutex.Lock()
		lastSentTime, found := lastNotificationSent[tcin]
		cooldownDuration := time.Duration(notificationCooldownMinutes) * time.Minute
		if found && time.Since(lastSentTime) < cooldownDuration {
			log.Printf("TCIN %s: IN STOCK but notification suppressed due to cooldown (last sent: %s, remaining: %s). Title: %s",
				product.TCIN, lastSentTime.Format(time.RFC1123), (cooldownDuration - time.Since(lastSentTime)).Round(time.Second), cleanedTitle)
			lastNotificationMutex.Unlock()
		} else {
			log.Printf("TCIN %s: %sIN STOCK!%s Title: %s. Qty: %.0f. Preparing notification.",
				product.TCIN, colorGreen, colorReset, cleanedTitle, availableQty)
			lastNotificationSent[tcin] = time.Now()
			lastNotificationMutex.Unlock()

			if discordWebhookURL != "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE" && !strings.Contains(discordWebhookURL, "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE") {
				sendDiscordNotification(cleanedTitle, product.Price.FormattedCurrentPrice, tcin, finalProductThumbnailURL, availableQty)
			} else {
				log.Printf("TCIN %s: Would send Discord notification, but webhook URL is not set.", product.TCIN)
			}

			// WebSocket Notification for high stock
			if availableQty >= highStockThreshold {
				productURL := fmt.Sprintf("https://www.target.com/p/-/A-%s", tcin)
				go broadcastHighStockNotification(product.TCIN, cleanedTitle, productURL, availableQty) // Run in a goroutine to avoid blocking
			}
		}

		recheckMutex.Lock()
		if _, ok := quickRecheckCounters[tcin]; ok {
			delete(quickRecheckCounters, tcin)
			log.Printf("TCIN %s: Item is IN_STOCK. Quick re-check schedule cleared.", product.TCIN)
		}
		recheckMutex.Unlock()

	} else { // Item is effectively OUT_OF_STOCK for notification purposes (includes PRE_ORDER_UNSELLABLE, etc.)
		log.Printf("TCIN %s: %s%s%s. Title: %s", // Use the coloredShippingStatus for the log
			product.TCIN, colorRed, shippingStatus, colorReset, cleanedTitle)
		if !isQuickRecheck && (!stateKnown || previousState) { // If it just transitioned to OOS or was unknown and is OOS
			recheckMutex.Lock()
			quickRecheckCounters[tcin] = quickRecheckCount
			log.Printf("TCIN %s: Item is effectively OUT OF STOCK (status: %s). Scheduled %d quick re-checks.", product.TCIN, shippingStatus, quickRecheckCount)
			recheckMutex.Unlock()
		}
	}
	return nil
}

// sendDiscordNotification places product image in the main Image field.
// Thumbnail is empty. Author icon is Target logo.
func sendDiscordNotification(title, price, tcin, productImageToDisplayURL string, availableQuantity float64) {
	if discordWebhookURL == "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE" || strings.Contains(discordWebhookURL, "YOUR_DISCORD_WEBHOOK_URL_HERE_PLEASE_UPDATE") || discordWebhookURL == "" {
		log.Println("Discord webhook URL not configured or placeholder. Skipping notification.")
		return
	}

	var messageContent string
	messageContent = ""

	// Timestamp formatting (as corrected previously)
	notificationTime := time.Now()
	var timestampStr string
	locToUse := estLocation
	if locToUse == nil {
		locToUse = time.Local
	}
	notificationTimeInLoc := notificationTime.In(locToUse)
	currentTimeInLoc := time.Now().In(locToUse)
	yNotif, mNotif, dNotif := notificationTimeInLoc.Date()
	yCurr, mCurr, dCurr := currentTimeInLoc.Date()
	timeOnlyFormatWithSeconds := "3:04:05 PM"
	fullDateTimeFormatWithSeconds := "Mon, Jan 2, 2006 at 3:04:05 PM"
	if yNotif == yCurr && mNotif == mCurr && dNotif == dCurr {
		timestampStr = fmt.Sprintf("Today at %s EST", notificationTimeInLoc.Format(timeOnlyFormatWithSeconds))
	} else {
		yesterday := currentTimeInLoc.AddDate(0, 0, -1)
		yYest, mYest, dYest := yesterday.Date()
		if yNotif == yYest && mNotif == mYest && dNotif == dYest {
			timestampStr = fmt.Sprintf("Yesterday at %s", notificationTimeInLoc.Format(timeOnlyFormatWithSeconds))
		} else {
			timestampStr = notificationTimeInLoc.Format(fullDateTimeFormatWithSeconds)
		}
	}

	colorGreen := 0x00FF00

	var displayQuantityStr string
	if availableQuantity == 0 {
		displayQuantityStr = "1+"
	} else {
		displayQuantityStr = fmt.Sprintf("%.0f", availableQuantity)
	}

	embedFields := []EmbedField{
		{Name: "Price", Value: price, Inline: true},
		{Name: "TCIN", Value: tcin, Inline: true},
		{Name: "Status", Value: "IN STOCK", Inline: false},
		{Name: "Available Quantity", Value: displayQuantityStr, Inline: false},
	}

	correctProductURL := fmt.Sprintf("https://www.target.com/p/-/A-%s", tcin)

	embed := DiscordEmbed{
		Title:     title,
		URL:       correctProductURL,
		Color:     colorGreen,
		Fields:    embedFields,
		Thumbnail: EmbedThumbnail{URL: ""},                   // Thumbnail (top-right) is empty
		Image:     EmbedImage{URL: productImageToDisplayURL}, // Product image in main image slot
		Footer: EmbedFooter{
			Text:    fmt.Sprintf("Targay Monitor | %s", timestampStr),
			IconURL: "",
		},
		Author: EmbedAuthor{
			Name:    "Targay Product Alert",
			URL:     "https://www.target.com",
			IconURL: discordEmbedImageURL,
		},
	}

	// --- DEBUG LOG FOR IMAGE URL ---
	log.Printf("DEBUG: Sending to Discord - Embed Image URL: '%s', Thumbnail URL: '%s'", embed.Image.URL, embed.Thumbnail.URL)
	// --- END DEBUG LOG ---

	message := DiscordMessage{
		Content:  messageContent,
		Username: "Targay Monitor",
		Embeds:   []DiscordEmbed{embed},
	}

	payload, err := json.Marshal(message)
	if err != nil {
		log.Printf("Failed to marshal Discord message for TCIN %s: %v", tcin, err)
		return
	}

	req, err := standard_http.NewRequest(standard_http.MethodPost, discordWebhookURL, bytes.NewBuffer(payload))
	if err != nil {
		log.Printf("Failed to create Discord request for TCIN %s: %v", tcin, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &standard_http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to send Discord notification for TCIN %s: %v", tcin, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("Successfully sent Discord notification for TCIN %s: %s (Qty: %.0f)", tcin, title, availableQuantity)
	} else {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("Discord notification for TCIN %s failed with status %d: %s", tcin, resp.StatusCode, string(bodyBytes))
	}
}

// logToFile writes a message to the designated API response log file.
// It uses a mutex to ensure thread-safe writes.
func logToFile(message string) {
	if !enableApiResponseLogging {
		return
	}
	apiResponseLogMutex.Lock()
	defer apiResponseLogMutex.Unlock()

	f, err := os.OpenFile(apiResponseLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("ERROR: API_LOG - Failed to open log file '%s': %v", apiResponseLogFile, err)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(message); err != nil {
		log.Printf("ERROR: API_LOG - Failed to write to log file '%s': %v", apiResponseLogFile, err)
	}
}

// logToWebSocketFile writes a message to the designated WebSocket log file.
func logToWebSocketFile(message string) {
	if !enableWebSocketLogging {
		return
	}
	webSocketLogMutex.Lock()
	defer webSocketLogMutex.Unlock()

	f, err := os.OpenFile(webSocketLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("ERROR: WEBSOCKET_LOG - Failed to open log file '%s': %v", webSocketLogFile, err)
		return
	}
	defer f.Close()

	// Add timestamp to the message before writing
	logEntry := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339Nano), message)
	if _, err := f.WriteString(logEntry); err != nil {
		log.Printf("ERROR: WEBSOCKET_LOG - Failed to write to log file '%s': %v", webSocketLogFile, err)
	}
}

// min helper function (if not already present or imported via math.Min with float64 conversion)
// func min(a, b int) int { if a < b { return a }; return b }

// loadTcinsFromFile reads TCINs from a text file, one TCIN per line.
// Lines starting with # and empty lines are ignored.
func loadTcinsFromFile(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("INFO: TCIN file '%s' not found. No TCINs will be monitored.", filePath)
			return nil, nil // Not a fatal error, just means no TCINs
		}
		return nil, fmt.Errorf("error opening TCIN file %s: %w", filePath, err)
	}
	defer file.Close()

	var loadedTcins []string
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") { // Skip empty lines and comments
			continue
		}
		// Basic validation: check if it looks like a TCIN (e.g., numeric and typical length)
		if matched, _ := regexp.MatchString(`^[0-9]{8,12}$`, line); !matched {
			log.Printf("WARN: Invalid TCIN format on line %d in %s: '%s' (skipped)", lineNumber, filePath, line)
			continue
		}
		loadedTcins = append(loadedTcins, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading TCIN file %s: %w", filePath, err)
	}

	if len(loadedTcins) == 0 {
		log.Printf("INFO: No valid TCINs found in '%s'.", filePath)
	} else {
		log.Printf("Successfully loaded %d TCINs to monitor from '%s'.", len(loadedTcins), filePath)
	}
	return loadedTcins, nil
}

// --- WebSocket Functions ---

func handleWebSocketConnections(w standard_http.ResponseWriter, r *standard_http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to upgrade connection: %v", err)
		log.Printf("ERROR: WEBSOCKET - %s", errMsg) // Keep critical error on console
		logToWebSocketFile("ERROR: " + errMsg)
		return
	}
	// defer conn.Close() // Closing is handled on error or disconnection

	wsClientsMutex.Lock()
	wsClients[conn] = true
	wsClientsMutex.Unlock()
	clientConnectedMsg := fmt.Sprintf("Client connected: %s. Total clients: %d", conn.RemoteAddr(), len(wsClients))
	log.Println("INFO: WEBSOCKET - " + clientConnectedMsg) // Optional: keep on console for visibility
	logToWebSocketFile(clientConnectedMsg)

	// Keep connection alive and detect disconnection
	// The voice bot might not send any messages, but this loop helps detect closure.
	for {
		// You can set a read deadline if you expect pings or want to timeout inactive connections
		// conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			wsClientsMutex.Lock()
			delete(wsClients, conn)
			wsClientsMutex.Unlock()

			var disconnectMsg string
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				disconnectMsg = fmt.Sprintf("Client %s disconnected with error: %v. Total clients: %d", conn.RemoteAddr(), err, len(wsClients))
				log.Printf("WARN: WEBSOCKET - %s", disconnectMsg) // Keep on console
			} else {
				disconnectMsg = fmt.Sprintf("Client %s disconnected. Total clients: %d", conn.RemoteAddr(), len(wsClients))
				log.Println("INFO: WEBSOCKET - " + disconnectMsg) // Optional: keep on console
			}
			logToWebSocketFile(disconnectMsg)
			_ = conn.Close() // Ensure connection is closed from server side
			break            // Exit the loop
		}
		// For now, we don't expect messages from the voice bot, but you could handle them here.
		receivedMsg := fmt.Sprintf("Received message from %s (type %d): %s", conn.RemoteAddr(), messageType, string(p))
		// log.Println("DEBUG: WEBSOCKET - " + receivedMsg) // Optional: console debug
		logToWebSocketFile("DEBUG: " + receivedMsg)
	}
}

func broadcastHighStockNotification(tcin, title, productURL string, quantity float64) {
	wsClientsMutex.Lock()
	defer wsClientsMutex.Unlock()

	if len(wsClients) == 0 {
		// logToWebSocketFile("No clients connected, skipping broadcast.") // Optional: reduce noise
		return
	}

	message := WebSocketMessage{
		TCIN:              tcin,
		Title:             title,
		AvailableQuantity: quantity,
		ProductURL:        productURL,
	}

	broadcastMsg := fmt.Sprintf("Broadcasting high stock for TCIN %s ('%s', Qty: %.0f) to %d clients.", tcin, title, quantity, len(wsClients))
	log.Println("INFO: WEBSOCKET - " + broadcastMsg) // Optional: keep on console
	logToWebSocketFile(broadcastMsg)

	for client := range wsClients {
		err := client.WriteJSON(message) // WriteJSON is convenient for sending structured data
		if err != nil {
			errorMsg := fmt.Sprintf("Error sending message to client %s: %v. Removing client.", client.RemoteAddr(), err)
			log.Printf("ERROR: WEBSOCKET - %s", errorMsg) // Keep error on console
			logToWebSocketFile("ERROR: " + errorMsg)
			_ = client.Close() // Close the connection on error
			delete(wsClients, client)
		}
	}
}

// --- End WebSocket Functions ---
