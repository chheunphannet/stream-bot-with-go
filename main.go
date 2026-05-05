package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	statsFile    = "stats.json"
	ajaxEndpoint = "https://khdiamond.net/wp-admin/admin-ajax.php"
	baseReferer  = "https://khdiamond.net"
)

var defaultHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
}

// ── Stats ──────────────────────────────────────────────────────────────────

type UserStat struct {
	Username     string `json:"username"`
	FirstSeen    string `json:"first_seen"`
	LastSeen     string `json:"last_seen"`
	RequestCount int    `json:"request_count"`
}

type Stats struct {
	mu            sync.Mutex
	TotalRequests int                 `json:"total_requests"`
	Users         map[string]UserStat `json:"users"`
}

func newStats() *Stats {
	return &Stats{
		Users: make(map[string]UserStat),
	}
}

func loadStats() *Stats {
	s := newStats()
	data, err := os.ReadFile(statsFile)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		return newStats()
	}
	if s.Users == nil {
		s.Users = make(map[string]UserStat)
	}
	return s
}

func (s *Stats) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		log.Println("Failed to marshal stats:", err)
		return
	}
	if err := os.WriteFile(statsFile, data, 0644); err != nil {
		log.Println("Failed to write stats:", err)
	}
}

func (s *Stats) trackUser(chatID int64, username string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%d", chatID)
	now := time.Now().UTC().Format(time.RFC3339)

	s.TotalRequests++

	u, exists := s.Users[key]
	if !exists {
		u = UserStat{
			Username:  username,
			FirstSeen: now,
		}
	}
	u.RequestCount++
	u.LastSeen = now
	if username != "" {
		u.Username = username
	}
	s.Users[key] = u
}

func (s *Stats) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	type entry struct {
		username string
		count    int
	}

	entries := make([]entry, 0, len(s.Users))
	for _, u := range s.Users {
		entries = append(entries, entry{u.Username, u.RequestCount})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].count > entries[j].count
	})

	top := entries
	if len(top) > 5 {
		top = top[:5]
	}

	lines := make([]string, len(top))
	for i, e := range top {
		lines[i] = fmt.Sprintf("%d. @%s — %d requests", i+1, e.username, e.count)
	}

	return fmt.Sprintf(
		"Stats:\n\nTotal Users: %d\nTotal Requests: %d\n\nTop 5 Users:\n%s",
		len(s.Users),
		s.TotalRequests,
		strings.Join(lines, "\n"),
	)
}

// ── HTTP Client ────────────────────────────────────────────────────────────

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func fetchHTML(pageURL, referer string) (string, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Referer", referer)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch %s (%d)", pageURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

var postIDRegex = regexp.MustCompile(`postid-(\d+)`)

type embedResponse struct {
	EmbedURL string `json:"embed_url"`
}

func getKhdiamondStream(pageURL, mediaType string) (string, error) {
	html, err := fetchHTML(pageURL, baseReferer)
	if err != nil {
		return "", err
	}

	matches := postIDRegex.FindStringSubmatch(html)
	if matches == nil {
		return "", fmt.Errorf("no post ID found on page")
	}
	postID := matches[1]

	form := url.Values{}
	form.Set("action", "doo_player_ajax")
	form.Set("post", postID)
	form.Set("nume", "1")
	form.Set("type", mediaType)

	req, err := http.NewRequest("POST", ajaxEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Referer", pageURL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ajax request failed (%d)", resp.StatusCode)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if result.EmbedURL == "" {
		return "", fmt.Errorf("no embed_url in response")
	}
	return result.EmbedURL, nil
}

// ── Pending Sessions ───────────────────────────────────────────────────────

type pendingMap struct {
	mu   sync.Mutex
	data map[int64]string
}

func newPendingMap() *pendingMap {
	return &pendingMap{data: make(map[int64]string)}
}

func (p *pendingMap) set(chatID int64, url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data[chatID] = url
}

func (p *pendingMap) get(chatID int64) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.data[chatID]
	return v, ok
}

func (p *pendingMap) delete(chatID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.data, chatID)
}

// ── Bot ────────────────────────────────────────────────────────────────────

func getUsername(from *tgbotapi.User) string {
	if from == nil {
		return "unknown"
	}
	if from.UserName != "" {
		return from.UserName
	}
	return from.FirstName
}

func main() {
	_ = godotenv.Load()

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN not set")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}

	log.Printf("Bot running as @%s", bot.Self.UserName)

	stats := loadStats()
	pending := newPendingMap()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Each update is handled in its own goroutine — fully non-blocking
	for update := range updates {
		go handleUpdate(bot, update, stats, pending)
	}
}

func handleUpdate(bot *tgbotapi.BotAPI, update tgbotapi.Update, stats *Stats, pending *pendingMap) {
	// ── Callback query (Movie / TV button press) ──
	if update.CallbackQuery != nil {
		handleCallback(bot, update.CallbackQuery, stats, pending)
		return
	}

	// ── Regular message ──
	if update.Message == nil {
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	if text == "" {
		return
	}

	// Commands
	if text == "/start" {
		send(bot, chatID, "Send me a khdiamond.net URL and I will get the stream for you.")
		return
	}

	if text == "/count_process" {
		send(bot, chatID, stats.report())
		return
	}

	if strings.HasPrefix(text, "/") {
		return
	}

	// URL handling
	if !strings.HasPrefix(text, "http") {
		send(bot, chatID, "Please send a valid URL.")
		return
	}
	if !strings.Contains(text, "khdiamond.net") {
		send(bot, chatID, "Only khdiamond.net URLs are supported.")
		return
	}

	pending.set(chatID, text)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Movie", "type_movie"),
			tgbotapi.NewInlineKeyboardButtonData("TV Show", "type_tv"),
		),
	)
	reply := tgbotapi.NewMessage(chatID, "Is this a Movie or TV Show?")
	reply.ReplyMarkup = keyboard
	bot.Send(reply)
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, stats *Stats, pending *pendingMap) {
	chatID := query.Message.Chat.ID
	username := getUsername(query.From)

	mediaType := "movie"
	if query.Data == "type_tv" {
		mediaType = "tv"
	}

	// Acknowledge button press
	bot.Request(tgbotapi.NewCallback(query.ID, ""))

	pageURL, ok := pending.get(chatID)
	if !ok {
		send(bot, chatID, "Session expired. Please send the URL again.")
		return
	}
	pending.delete(chatID)

	// Track request
	stats.trackUser(chatID, username)
	go stats.save() // save asynchronously

	// Loading message
	loadingMsg, _ := bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Fetching %s stream...", mediaType)))

	// Fetch stream — this is the heavy IO, runs in this goroutine (already non-blocking)
	embedURL, err := getKhdiamondStream(pageURL, mediaType)

	if err != nil {
		send(bot, chatID, fmt.Sprintf("Error: %s", err.Error()))
	} else {
		send(bot, chatID, fmt.Sprintf("Embed URL:\n%s", embedURL))
	}

	// Delete loading message
	bot.Request(tgbotapi.NewDeleteMessage(chatID, loadingMsg.MessageID))
}

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	bot.Send(tgbotapi.NewMessage(chatID, text))
}
