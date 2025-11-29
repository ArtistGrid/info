package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

var (
	originRegex = regexp.MustCompile(`^https://([a-zA-Z0-9-]+\.)*artistgrid\.cx$`)
	idSanitizer = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	debugMode   bool
)

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	},
}

type ImgurFile struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name,omitempty"`
	OriginalName  string `json:"originalName,omitempty"`
	Size          int64  `json:"size,omitempty"`
	MimeType      string `json:"mimeType,omitempty"`
	DownloadCount int    `json:"downloadCount,omitempty"`
	UploadedAt    string `json:"uploadedAt,omitempty"`
	BucketKey     string `json:"bucketKey,omitempty"`
}

type ImgurMetadata struct {
	Title    string      `json:"title,omitempty"`
	Artist   string      `json:"artist,omitempty"`
	Album    string      `json:"album,omitempty"`
	Track    interface{} `json:"track,omitempty"`
	Duration float64     `json:"duration,omitempty"`
	Bitrate  float64     `json:"bitrate,omitempty"`
	Cover    string      `json:"cover,omitempty"`
	Comment  interface{} `json:"comment,omitempty"`
}

type ApiResponse struct {
	Success   bool           `json:"success"`
	ID        string         `json:"id,omitempty"`
	Source    string         `json:"source,omitempty"`
	Title     string         `json:"title,omitempty"`
	Size      string         `json:"size,omitempty"`
	Type      string         `json:"type,omitempty"`
	MP3       string         `json:"mp3,omitempty"`
	M4A       string         `json:"m4a,omitempty"`
	File      *ImgurFile     `json:"file,omitempty"`
	Metadata  *ImgurMetadata `json:"metadata,omitempty"`
	Cover     string         `json:"cover,omitempty"`
	Views     string         `json:"views,omitempty"`
	Downloads string         `json:"downloads,omitempty"`
	Uploaded  string         `json:"uploaded,omitempty"`
	Waveform  string         `json:"waveform,omitempty"`
	Spectrum  string         `json:"spectrum,omitempty"`
	Error     string         `json:"error,omitempty"`
	Cached    bool           `json:"cached"`
}

type CacheEntry struct {
	Data       []byte
	LastAccess time.Time
}

type Cache struct {
	redis    *redis.Client
	useRedis bool
	memory   sync.Map
	ttl      time.Duration
	sfGroup  singleflight.Group
}

func NewCache() *Cache {
	c := &Cache{
		ttl: 10 * time.Hour,
	}

	if debugMode {
		log.Println("[CACHE] Debug mode enabled - caching disabled")
		return c
	}

	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err == nil {
			opt.PoolSize = 50
			opt.MinIdleConns = 10
			opt.DialTimeout = 5 * time.Second
			opt.ReadTimeout = 3 * time.Second
			opt.WriteTimeout = 3 * time.Second
			opt.PoolTimeout = 4 * time.Second

			client := redis.NewClient(opt)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := client.Ping(ctx).Err(); err == nil {
				c.redis = client
				c.useRedis = true
				log.Println("[CACHE] Redis connected - indefinite TTL enabled")
			} else {
				log.Printf("[CACHE] Redis failed: %v - using in-memory", err)
			}
		} else {
			log.Printf("[CACHE] Redis URL parse error: %v", err)
		}
	}

	if !c.useRedis {
		log.Println("[CACHE] In-memory cache - 10h TTL from last access")
		go c.cleanup()
	}

	return c
}

func (c *Cache) cleanup() {
	ticker := time.NewTicker(30 * time.Minute)
	for range ticker.C {
		now := time.Now()
		c.memory.Range(func(key, value interface{}) bool {
			if entry, ok := value.(*CacheEntry); ok {
				if now.Sub(entry.LastAccess) > c.ttl {
					c.memory.Delete(key)
				}
			}
			return true
		})
	}
}

func (c *Cache) Get(ctx context.Context, prefix, key string) (*ApiResponse, bool) {
	if debugMode {
		return nil, false
	}

	cacheKey := prefix + ":" + key

	if c.useRedis {
		data, err := c.redis.Get(ctx, cacheKey).Bytes()
		if err == nil {
			var resp ApiResponse
			if json.Unmarshal(data, &resp) == nil {
				resp.Cached = true
				return &resp, true
			}
		}
		return nil, false
	}

	if val, ok := c.memory.Load(cacheKey); ok {
		entry := val.(*CacheEntry)
		entry.LastAccess = time.Now()
		var resp ApiResponse
		if json.Unmarshal(entry.Data, &resp) == nil {
			resp.Cached = true
			return &resp, true
		}
	}
	return nil, false
}

func (c *Cache) Set(ctx context.Context, prefix, key string, resp *ApiResponse) {
	if debugMode {
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	cacheKey := prefix + ":" + key

	if c.useRedis {
		c.redis.Set(ctx, cacheKey, data, 0)
		return
	}

	c.memory.Store(cacheKey, &CacheEntry{
		Data:       data,
		LastAccess: time.Now(),
	})
}

type RateLimiter struct {
	limiters sync.Map
	rate     rate.Limit
	burst    int
}

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		rate:  rate.Limit(rps),
		burst: burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		rl.limiters.Range(func(key, _ interface{}) bool {
			rl.limiters.Delete(key)
			return true
		})
	}
}

func (rl *RateLimiter) Allow(ip string) bool {
	if debugMode {
		return true
	}
	val, _ := rl.limiters.LoadOrStore(ip, rate.NewLimiter(rl.rate, rl.burst))
	return val.(*rate.Limiter).Allow()
}

var (
	cache       *Cache
	rateLimiter *RateLimiter
)

func isAllowedOrigin(origin string) bool {
	if debugMode {
		return true
	}
	return origin != "" && originRegex.MatchString(origin)
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, resp *ApiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		if origin != "" {
			if !isAllowedOrigin(origin) {
				writeJSON(w, http.StatusForbidden, &ApiResponse{
					Success: false,
					Error:   "Origin not allowed",
				})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, &ApiResponse{
				Success: false,
				Error:   "Method not allowed",
			})
			return
		}

		next(w, r)
	}
}

func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := getClientIP(r)
		if !rateLimiter.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, &ApiResponse{
				Success: false,
				Error:   "Rate limit exceeded",
			})
			return
		}
		next(w, r)
	}
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func extractImgurDataAdvanced(content string) (*ImgurFile, *ImgurMetadata) {
	var file *ImgurFile
	var metadata *ImgurMetadata

	filePattern := regexp.MustCompile(`\{"id":"([^"]+)","name":"([^"]+)","originalName":"([^"]+)","size":(\d+),"mimeType":"([^"]+)","downloadCount":(\d+),"uploadedAt":"([^"]+)"[^}]*"bucketKey":"([^"]+)"[^}]*\}`)

	if match := filePattern.FindStringSubmatch(content); len(match) > 8 {
		size, _ := strconv.ParseInt(match[4], 10, 64)
		downloadCount, _ := strconv.Atoi(match[6])
		file = &ImgurFile{
			ID:            match[1],
			Name:          match[2],
			OriginalName:  match[3],
			Size:          size,
			MimeType:      match[5],
			DownloadCount: downloadCount,
			UploadedAt:    match[7],
			BucketKey:     match[8],
		}
	}

	metaPattern := regexp.MustCompile(`"metadata":\{"title":"([^"]*)"(?:,"artist":"([^"]*)")?(?:,"album":"([^"]*)")?[^}]*"duration":([0-9.]+)[^}]*"bitrate":([0-9.]+)[^}]*"cover":"([^"]*)"`)

	if match := metaPattern.FindStringSubmatch(content); len(match) > 6 {
		duration, _ := strconv.ParseFloat(match[4], 64)
		bitrate, _ := strconv.ParseFloat(match[5], 64)
		metadata = &ImgurMetadata{
			Title:    match[1],
			Artist:   match[2],
			Album:    match[3],
			Duration: duration,
			Bitrate:  bitrate,
			Cover:    match[6],
		}
	}

	return file, metadata
}

func fetchFromImgur(ctx context.Context, id string) (*ApiResponse, error) {
	url := "https://imgur.gg/f/" + id

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Failed to create request"}, nil
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := httpClient.Do(req)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Upstream request failed"}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &ApiResponse{Success: false, ID: id, Error: "Not found"}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return &ApiResponse{Success: false, Error: "Upstream error: " + resp.Status}, nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Failed to parse HTML"}, nil
	}

	apiResp := &ApiResponse{
		Success: true,
		ID:      id,
		Source:  "imgur",
		Cached:  false,
	}

	mp3, _ := doc.Find("audio").Attr("src")
	apiResp.MP3 = mp3

	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		if src, exists := s.Attr("src"); exists {
			if strings.Contains(src, "covers/") || strings.Contains(src, "i.imgur.gg/covers") {
				apiResp.Cover = src
			}
		}
	})

	pageTitle := doc.Find("title").First().Text()
	pageTitle = strings.TrimSuffix(pageTitle, " - imgur.gg")
	apiResp.Title = strings.TrimSpace(pageTitle)

	var file *ImgurFile
	var metadata *ImgurMetadata

	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		text := s.Text()

		if strings.Contains(text, `"file":{`) || strings.Contains(text, `"metadata":{`) || strings.Contains(text, "self.__next_f.push") {
			f, m := extractImgurDataAdvanced(text)
			if f != nil && file == nil {
				file = f
			}
			if m != nil && metadata == nil {
				metadata = m
			}
		}
	})

	if file != nil {
		apiResp.File = file
		if apiResp.Title == "" && file.OriginalName != "" {
			apiResp.Title = file.OriginalName
		}
		if file.Size > 0 {
			apiResp.Size = formatBytes(file.Size)
		}
		if file.MimeType != "" {
			apiResp.Type = file.MimeType
		}
	}

	if metadata != nil {
		apiResp.Metadata = metadata
		if metadata.Cover != "" && apiResp.Cover == "" {
			apiResp.Cover = metadata.Cover
		}
		if metadata.Title != "" {
			apiResp.Title = metadata.Title
			if metadata.Artist != "" {
				apiResp.Title = metadata.Artist + " - " + metadata.Title
			}
		}
	}

	return apiResp, nil
}

func fetchFromKrakenFiles(ctx context.Context, id string) (*ApiResponse, error) {
	url := "https://krakenfiles.com/view/" + id + "/file.html"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Failed to create request"}, nil
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ArtistGridBot/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := httpClient.Do(req)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Upstream request failed"}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &ApiResponse{Success: false, ID: id, Error: "Not found"}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return &ApiResponse{Success: false, Error: "Upstream error: " + resp.Status}, nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return &ApiResponse{Success: false, Error: "Failed to parse HTML"}, nil
	}

	apiResp := &ApiResponse{
		Success: true,
		ID:      id,
		Source:  "krakenfiles",
		Cached:  false,
	}

	title := doc.Find("title").First().Text()
	title = strings.TrimSuffix(title, " - Krakenfiles.com")
	apiResp.Title = strings.TrimSpace(title)

	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		text := s.Text()
		if strings.Contains(text, "jPlayer") && strings.Contains(text, "m4a:") {
			m4aRegex := regexp.MustCompile(`m4a:\s*['"]([^'"]+)['"]`)
			if match := m4aRegex.FindStringSubmatch(text); len(match) > 1 {
				m4aURL := match[1]
				if strings.HasPrefix(m4aURL, "//") {
					m4aURL = "https:" + m4aURL
				}
				apiResp.M4A = m4aURL
			}
		}
	})

	waveformImg := doc.Find(".jp-seek-bar img")
	if src, exists := waveformImg.Attr("src"); exists {
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		apiResp.Waveform = src
	}

	spectrumImg := doc.Find("#spectrum-gallery a")
	if href, exists := spectrumImg.Attr("href"); exists {
		if strings.HasPrefix(href, "//") {
			href = "https:" + href
		}
		apiResp.Spectrum = href
	}

	doc.Find(".nk-iv-wg4-overview li").Each(func(_ int, s *goquery.Selection) {
		label := strings.TrimSpace(s.Find(".sub-text").Text())
		value := strings.TrimSpace(s.Find(".lead-text").Text())

		switch strings.ToLower(label) {
		case "upload date":
			apiResp.Uploaded = value
		case "file size":
			apiResp.Size = value
		case "type":
			apiResp.Type = value
		}
	})

	doc.Find(".nk-iv-wg4-list li").Each(func(_ int, s *goquery.Selection) {
		label := strings.TrimSpace(s.Find(".views-count").First().Text())
		if strings.ToLower(label) == "views" {
			apiResp.Views = strings.TrimSpace(s.Find(".views-count").Last().Text())
		}
		labelDl := strings.TrimSpace(s.Find(".downloads-count").First().Text())
		if strings.ToLower(labelDl) == "downloads" {
			apiResp.Downloads = strings.TrimSpace(s.Find(".downloads-count strong").Text())
		}
	})

	return apiResp, nil
}

func imgurHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, &ApiResponse{
			Success: false,
			Error:   "Missing ?id parameter",
		})
		return
	}

	id = strings.TrimSpace(id)
	if len(id) > 64 || !idSanitizer.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, &ApiResponse{
			Success: false,
			Error:   "Invalid ID format",
		})
		return
	}

	ctx := r.Context()
	cachePrefix := "imgur"

	if cached, ok := cache.Get(ctx, cachePrefix, id); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	result, err, _ := cache.sfGroup.Do(cachePrefix+":"+id, func() (interface{}, error) {
		if cached, ok := cache.Get(ctx, cachePrefix, id); ok {
			return cached, nil
		}

		resp, err := fetchFromImgur(ctx, id)
		if err != nil {
			return nil, err
		}

		if resp.Success {
			cache.Set(ctx, cachePrefix, id, resp)
		}

		return resp, nil
	})

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, &ApiResponse{
			Success: false,
			Error:   "Internal error",
		})
		return
	}

	resp := result.(*ApiResponse)
	status := http.StatusOK
	if !resp.Success {
		if resp.Error == "Not found" {
			status = http.StatusNotFound
		} else {
			status = http.StatusBadGateway
		}
	}

	writeJSON(w, status, resp)
}

func kfHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, &ApiResponse{
			Success: false,
			Error:   "Missing ?id parameter",
		})
		return
	}

	id = strings.TrimSpace(id)
	if len(id) > 64 || !idSanitizer.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, &ApiResponse{
			Success: false,
			Error:   "Invalid ID format",
		})
		return
	}

	ctx := r.Context()
	cachePrefix := "kf"

	if cached, ok := cache.Get(ctx, cachePrefix, id); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	result, err, _ := cache.sfGroup.Do(cachePrefix+":"+id, func() (interface{}, error) {
		if cached, ok := cache.Get(ctx, cachePrefix, id); ok {
			return cached, nil
		}

		resp, err := fetchFromKrakenFiles(ctx, id)
		if err != nil {
			return nil, err
		}

		if resp.Success {
			cache.Set(ctx, cachePrefix, id, resp)
		}

		return resp, nil
	})

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, &ApiResponse{
			Success: false,
			Error:   "Internal error",
		})
		return
	}

	resp := result.(*ApiResponse)
	status := http.StatusOK
	if !resp.Success {
		if resp.Error == "Not found" {
			status = http.StatusNotFound
		} else {
			status = http.StatusBadGateway
		}
	}

	writeJSON(w, status, resp)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"cache":     "memory",
		"debug":     debugMode,
		"endpoints": []string{"/imgur/", "/kf/"},
	}

	if debugMode {
		status["cache"] = "disabled"
	} else if cache.useRedis {
		status["cache"] = "redis"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cache.redis.Ping(ctx).Err(); err != nil {
			status["redis_status"] = "degraded"
		} else {
			status["redis_status"] = "connected"
		}
	}

	json.NewEncoder(w).Encode(status)
}

func init() {
	if err := godotenv.Load(); err != nil {
		log.Println("[ENV] No .env file found, using system environment")
	} else {
		log.Println("[ENV] Loaded .env file")
	}

	debugEnv := strings.ToLower(os.Getenv("DEBUG"))
	debugMode = debugEnv == "true" || debugEnv == "1"
}

func main() {
	cache = NewCache()
	rateLimiter = NewRateLimiter(30, 60)

	http.HandleFunc("/imgur/", corsMiddleware(rateLimitMiddleware(imgurHandler)))
	http.HandleFunc("/kf/", corsMiddleware(rateLimitMiddleware(kfHandler)))
	http.HandleFunc("/health", healthHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	server := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		log.Printf("[SERVER] Starting on :%s", port)
		log.Printf("[SERVER] Endpoints: /imgur/?id=X, /kf/?id=X")
		log.Printf("[SERVER] Debug mode: %v", debugMode)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[SERVER] Fatal: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[SERVER] Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[SERVER] Shutdown error: %v", err)
	}

	if cache.useRedis && cache.redis != nil {
		if err := cache.redis.Close(); err != nil {
			log.Printf("[CACHE] Redis close error: %v", err)
		}
	}

	log.Println("[SERVER] Stopped")
}
