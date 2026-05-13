package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	dbFile       = "stats.db"
	ajaxEndpoint = "https://khdiamond.net/wp-admin/admin-ajax.php"
	baseReferer  = "https://khdiamond.net"
)

var defaultHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
}

// ── Database & Stats ───────────────────────────────────────────────────────

type Stats struct {
	db *sql.DB
}

func initDB() *Stats {
	// Open with WAL mode and busy timeout for concurrent safety
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbFile)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

	// Optimize for single-writer
	db.SetMaxOpenConns(1)

	query := `
	CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		username TEXT,
		first_seen DATETIME,
		last_seen DATETIME,
		request_count INTEGER DEFAULT 0,
		last_url TEXT,
		last_msg_id INTEGER,
		next_url TEXT
	);`
	if _, err := db.Exec(query); err != nil {
		log.Fatal("Failed to create table:", err)
	}

	// Migration: Ensure new columns exist
	_, _ = db.Exec("ALTER TABLE users ADD COLUMN last_msg_id INTEGER;")
	_, _ = db.Exec("ALTER TABLE users ADD COLUMN next_url TEXT;")

	return &Stats{db: db}
}

func (s *Stats) trackUser(chatID int64, username string, pageURL string) {
	key := fmt.Sprintf("%d", chatID)
	now := time.Now().UTC().Format(time.RFC3339)

	query := `
	INSERT INTO users (id, username, first_seen, last_seen, request_count, last_url)
	VALUES (?, ?, ?, ?, 1, ?)
	ON CONFLICT(id) DO UPDATE SET
		username = excluded.username,
		last_seen = excluded.last_seen,
		request_count = request_count + 1,
		last_url = excluded.last_url;
	`
	_, err := s.db.Exec(query, key, username, now, now, pageURL)
	if err != nil {
		log.Println("Database error (trackUser):", err)
	}
}

func (s *Stats) updateLastMessage(chatID int64, msgID int) {
	query := "UPDATE users SET last_msg_id = ? WHERE id = ?"
	s.db.Exec(query, msgID, fmt.Sprintf("%d", chatID))
}

func (s *Stats) updateNextURL(chatID int64, nextURL string) {
	query := "UPDATE users SET next_url = ? WHERE id = ?"
	s.db.Exec(query, nextURL, fmt.Sprintf("%d", chatID))
}

func (s *Stats) getLastInfo(chatID int64) (string, string, int, error) {
	var lastURL string
	var nextURL sql.NullString
	var lastMsgID sql.NullInt64
	err := s.db.QueryRow("SELECT last_url, next_url, last_msg_id FROM users WHERE id = ?", fmt.Sprintf("%d", chatID)).Scan(&lastURL, &nextURL, &lastMsgID)

	msgID := 0
	if lastMsgID.Valid {
		msgID = int(lastMsgID.Int64)
	}

	nURL := ""
	if nextURL.Valid {
		nURL = nextURL.String
	}

	return lastURL, nURL, msgID, err
}

func (s *Stats) report() string {
	var totalUsers int
	var totalRequests int

	s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(request_count), 0) FROM users").Scan(&totalUsers, &totalRequests)

	rows, err := s.db.Query("SELECT username, request_count FROM users ORDER BY request_count DESC LIMIT 5")
	if err != nil {
		return "Error generating report"
	}
	defer rows.Close()

	var lines []string
	i := 1
	for rows.Next() {
		var uname string
		var count int
		rows.Scan(&uname, &count)
		lines = append(lines, fmt.Sprintf("%d. @%s — %d requests", i, uname, count))
		i++
	}

	return fmt.Sprintf(
		"Stats:\n\nTotal Users: %d\nTotal Requests: %d\n\nTop 5 Users:\n%s",
		totalUsers,
		totalRequests,
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
	Type     any    `json:"type"`
}

var nextURLRegex = regexp.MustCompile(`<a href=['"]([^'"]+)['"][^>]*>\s*<span>ភាគបន្ទាប់</span>`)

var (
	titleRegex   = regexp.MustCompile(`<h1[^>]*>([^<]+)</h1>`)
	numRegex     = regexp.MustCompile(`(\d+)x(\d+)|S(\d+)\s*-\s*E(\d+)`)
	cleanupRegex = regexp.MustCompile(`[:\-\s]+$`)
)

var errPageNotAvailable = errors.New("page not available")

func cleanEnglishTitle(rawTitle string) string {
	title := html.UnescapeString(strings.TrimSpace(rawTitle))
	for _, separator := range []string{"\u2013", "\u2014"} {
		parts := strings.Split(title, separator)
		if len(parts) > 1 {
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return title
}

func getKhdiamondStream(pageURL string) (string, string, string, error) {
	html, err := fetchHTML(pageURL, baseReferer)
	if err != nil {
		return "", "", "", err
	}

	// Detect media type
	mediaType := "movie"
	if strings.Contains(html, "single-episodes") || strings.Contains(html, "single-tvshows") || strings.Contains(pageURL, "/episodes/") || strings.Contains(pageURL, "/tvshows/") {
		mediaType = "tv"
	}

	// Extract Title
	title := "Unknown"
	titleMatches := titleRegex.FindStringSubmatch(html)
	if len(titleMatches) > 1 {
		title = cleanEnglishTitle(titleMatches[1])
	}

	// Extract Next URL
	nextURL := ""
	if mediaType == "tv" {
		nextMatches := nextURLRegex.FindStringSubmatch(html)
		if len(nextMatches) > 1 {
			nextURL = resolvePageURL(nextMatches[1])
		}
	}

	// Parse Season/Episode and clean title
	metaInfo := ""
	if mediaType == "tv" {
		numMatches := numRegex.FindStringSubmatch(title)
		if len(numMatches) == 0 {
			// Try finding in the 'numerando' div if title doesn't have it
			numMatches = numRegex.FindStringSubmatch(html)
		}
		if len(numMatches) == 0 {
			numMatches = numRegex.FindStringSubmatch(pageURL)
		}

		if len(numMatches) > 0 {
			s, e := "", ""
			if numMatches[1] != "" { // 1x1 format
				s, e = numMatches[1], numMatches[2]
			} else { // S1 - E1 format
				s, e = numMatches[3], numMatches[4]
			}
			metaInfo = fmt.Sprintf("\nSeason: %s Episode: %s", s, e)

			// Remove the episode part from title for display if it's there
			title = numRegex.ReplaceAllString(title, "")
			title = cleanupRegex.ReplaceAllString(title, "")
		}
	}

	displayTitle := fmt.Sprintf("Title: %s%s", title, metaInfo)

	matches := postIDRegex.FindStringSubmatch(html)
	if matches == nil {
		return "", "", "", errPageNotAvailable
	}
	postID := matches[1]

	form := url.Values{}
	form.Set("action", "doo_player_ajax")
	form.Set("post", postID)
	form.Set("nume", "1")
	form.Set("type", mediaType)

	req, err := http.NewRequest("POST", ajaxEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", "", err
	}
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Referer", pageURL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("ajax request failed (%d)", resp.StatusCode)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", fmt.Errorf("failed to parse response: %w", err)
	}
	if result.EmbedURL == "" {
		return "", "", "", fmt.Errorf("no embed_url in response")
	}

	if strings.Contains(result.EmbedURL, "/error/") || result.Type == false {
		if mediaType == "tv" {
			return "", "", "", fmt.Errorf("this looks like a TV Show main page. Please send a link to a specific episode")
		}
		return "", "", "", fmt.Errorf("the website returned an error. Please ensure the URL is valid")
	}

	return result.EmbedURL, nextURL, displayTitle, nil
}

func resolvePageURL(href string) string {
	parsed, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return href
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	base, err := url.Parse(baseReferer)
	if err != nil {
		return href
	}
	return base.ResolveReference(parsed).String()
}

func userErrorMessage(err error) string {
	if errors.Is(err, errPageNotAvailable) {
		return "This episode is not available."
	}
	return fmt.Sprintf("Error: %s", err.Error())
}

func getUsername(from *tgbotapi.User) string {
	if from == nil {
		return "unknown"
	}
	if from.UserName != "" {
		return from.UserName
	}
	if from.FirstName != "" {
		return from.FirstName
	}
	return "unknown"
}

func isKhdiamondURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "khdiamond.net" || strings.HasSuffix(host, ".khdiamond.net")
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

	stats := initDB()
	defer stats.db.Close()

	log.Printf("Bot running as @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		go handleUpdate(bot, update, stats)
	}
}

func handleUpdate(bot *tgbotapi.BotAPI, update tgbotapi.Update, stats *Stats) {
	if update.CallbackQuery != nil {
		handleCallback(bot, update.CallbackQuery, stats)
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	switch text {
	case "/start":
		send(bot, chatID, "Send me a khdiamond.net URL and I will get the stream for you.")
		return
	case "/count_process":
		send(bot, chatID, stats.report())
		return
	}

	if strings.HasPrefix(text, "/") {
		return
	}
	if !isKhdiamondURL(text) {
		send(bot, chatID, "Please send a valid khdiamond.net URL.")
		return
	}

	go processRequest(bot, chatID, text, getUsername(msg.From), stats)
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, stats *Stats) {
	if query.Message == nil {
		return
	}

	chatID := query.Message.Chat.ID
	username := getUsername(query.From)
	bot.Request(tgbotapi.NewCallback(query.ID, ""))

	lastURL, nextURL, _, err := stats.getLastInfo(chatID)
	if err != nil {
		send(bot, chatID, "Please send a khdiamond.net URL first.")
		return
	}

	switch query.Data {
	case "refresh":
		if lastURL == "" {
			send(bot, chatID, "Please send a khdiamond.net URL first.")
			return
		}
		go processRequest(bot, chatID, lastURL, username, stats)
	case "next":
		if nextURL == "" {
			send(bot, chatID, "No next episode found for this page.")
			return
		}
		go processRequest(bot, chatID, nextURL, username, stats)
	}
}

func processRequest(bot *tgbotapi.BotAPI, chatID int64, pageURL string, username string, stats *Stats) {
	// Remove old buttons
	_, _, oldMsgID, _ := stats.getLastInfo(chatID)
	if oldMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, oldMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		bot.Send(edit)
	}

	stats.trackUser(chatID, username, pageURL)
	loadingMsg, _ := bot.Send(tgbotapi.NewMessage(chatID, "Analyzing URL and fetching stream..."))

	embedURL, nextURL, displayTitle, err := getKhdiamondStream(pageURL)

	if err != nil {
		send(bot, chatID, userErrorMessage(err))
	} else {
		stats.updateNextURL(chatID, nextURL)

		msgText := fmt.Sprintf("%s\n\nEmbed URL:\n%s", displayTitle, embedURL)
		msg := tgbotapi.NewMessage(chatID, msgText)

		var row []tgbotapi.InlineKeyboardButton
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh Link", "refresh"))
		if nextURL != "" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("⏭ Next Episode", "next"))
		}

		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(row...))
		sentMsg, _ := bot.Send(msg)
		stats.updateLastMessage(chatID, sentMsg.MessageID)
	}

	bot.Request(tgbotapi.NewDeleteMessage(chatID, loadingMsg.MessageID))
}

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	bot.Send(tgbotapi.NewMessage(chatID, text))
}
