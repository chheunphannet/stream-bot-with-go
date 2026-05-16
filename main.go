package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	dbFile            = "stats.db"
	ajaxEndpoint      = "https://khdiamond.net/wp-admin/admin-ajax.php"
)

// EXACT User-Agent from your Laravel StreamService.php
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"

// ── Database & Stats ───────────────────────────────────────────────────────

type Stats struct {
	db *sql.DB
}

func initDB() *Stats {
	_ = godotenv.Load()
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbFile)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

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

	// Migration
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
	_ = s.db.QueryRow("SELECT COUNT(*), SUM(request_count) FROM users").Scan(&totalUsers, &totalRequests)

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

// ── HTTP Client (cURL Wrapper - Minimalist Laravel Style) ──────────────────

type httpStatusError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *httpStatusError) Error() string {
	if e.StatusCode == 403 {
		return fmt.Sprintf("HTTP 403 Forbidden from %s. The source site denied this server request", e.URL)
	}
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.URL)
}

func doRequest(method, targetURL, referer string, bodyStr string) (string, error) {
	return doRequestViaSolverr(method, targetURL, referer, bodyStr)
}

func doRequestViaSolverr(method, targetURL, referer string, bodyStr string) (string, error) {
	solverrURL := os.Getenv("FLARESOLVERR_URL")
	if solverrURL == "" {
		solverrURL = "http://localhost:8191/v1"
	}

	cmd := "request.get"
	if method == "POST" {
		cmd = "request.post"
	}

	payload := map[string]any{
		"cmd":        cmd,
		"url":        targetURL,
		"maxTimeout": 60000,
	}

	if method == "POST" {
		payload["postData"] = bodyStr
	}

	// Add headers if provided
	headers := map[string]string{
		"User-Agent": userAgent,
	}
	if referer != "" {
		headers["Referer"] = referer
	}
	payload["headers"] = headers

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	resp, err := http.Post(solverrURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			Response string `json:"response"`
			Status   int    `json:"status"`
			Cookies  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Status == "error" {
		return "", fmt.Errorf("FlareSolverr error: %s", result.Message)
	}

	if result.Solution.Status >= 400 {
		return result.Solution.Response, &httpStatusError{
			StatusCode: result.Solution.Status,
			URL:        targetURL,
			Body:       result.Solution.Response,
		}
	}

	return result.Solution.Response, nil
}

func fetchHTML(pageURL, referer string) (string, error) {
	return doRequestViaSolverr("GET", pageURL, referer, "")
}

func bodySnippet(body string, limit int) string {
	body = strings.TrimSpace(body)
	if len(body) > limit {
		return body[:limit]
	}
	return body
}

func isCloudflareBlock(html string) bool {
	lower := strings.ToLower(html)
	markers := []string{
		"just a moment...",
		"cf-challenge",
		"cf-error-code",
		"cloudflare ray id",
		"attention required! | cloudflare",
		"/cdn-cgi/challenge-platform/",
	}

	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	return false
}

func sourceAccessError(err error) error {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return nil
	}

	if statusErr.StatusCode == 403 || isCloudflareBlock(statusErr.Body) {
		log.Printf("DEBUG: Source access denied. Status=%d Body=%s", statusErr.StatusCode, bodySnippet(statusErr.Body, 300))
		return fmt.Errorf("source site returned HTTP %d. Ensure FlareSolverr is running and check its logs for blocks or captcha challenges", statusErr.StatusCode)
	}

	return nil
}

var (
	postIDRegex  = regexp.MustCompile(`postid-(\d+)`)
	nextURLRegex = regexp.MustCompile(`<a href=['"]([^'"]+)['"][^>]*>\s*<span>ភាគបន្ទាប់</span>`)
	titleRegex   = regexp.MustCompile(`<h1[^>]*>([^<]+)</h1>`)
	numRegex     = regexp.MustCompile(`(\d+)x(\d+)|S(\d+)\s*-\s*E(\d+)`)
	cleanupRegex = regexp.MustCompile(`[:\-\s]+$`)
)

type embedResponse struct {
	EmbedURL string `json:"embed_url"`
	Type     any    `json:"type"`
}

func getKhdiamondStream(pageURL string) (string, string, string, error) {
	// Match Laravel's fetchHtml($url, "https://$host")
	u, _ := url.Parse(pageURL)
	html, err := fetchHTML(pageURL, fmt.Sprintf("%s://%s", u.Scheme, u.Host))
	if err != nil {
		if accessErr := sourceAccessError(err); accessErr != nil {
			return "", "", "", accessErr
		}
		return "", "", "", err
	}

	// Cloudflare detection
	if isCloudflareBlock(html) {
		log.Printf("DEBUG: Blocked by Cloudflare. Received: %s", bodySnippet(html, 300))
		return "", "", "", fmt.Errorf("Cloudflare challenge detected. This VPS IP might be flagged")
	}

	mediaType := "movie"
	if strings.Contains(html, "single-episodes") || strings.Contains(html, "single-tvshows") || strings.Contains(pageURL, "/episodes/") || strings.Contains(pageURL, "/tvshows/") {
		mediaType = "tv"
	}

	title := "Unknown"
	titleMatches := titleRegex.FindStringSubmatch(html)
	if len(titleMatches) > 1 {
		title = strings.TrimSpace(titleMatches[1])
	}

	nextURL := ""
	if mediaType == "tv" {
		nextMatches := nextURLRegex.FindStringSubmatch(html)
		if len(nextMatches) > 1 {
			nextURL = nextMatches[1]
		}
	}

	metaInfo := ""
	if mediaType == "tv" {
		numMatches := numRegex.FindStringSubmatch(title)
		if len(numMatches) == 0 {
			numMatches = numRegex.FindStringSubmatch(html)
		}

		if len(numMatches) > 0 {
			s, e := "", ""
			if numMatches[1] != "" {
				s, e = numMatches[1], numMatches[2]
			} else {
				s, e = numMatches[3], numMatches[4]
			}
			metaInfo = fmt.Sprintf("\nSeason: %s, Ep: %s", s, e)
			title = numRegex.ReplaceAllString(title, "")
			title = cleanupRegex.ReplaceAllString(title, "")
		}
	}

	displayTitle := fmt.Sprintf("Title: %s%s", title, metaInfo)

	matches := postIDRegex.FindStringSubmatch(html)
	if matches == nil {
		return "", "", "", fmt.Errorf("no post ID found on page. (Site might have changed structure)")
	}
	postID := matches[1]

	form := url.Values{}
	form.Set("action", "doo_player_ajax")
	form.Set("post", postID)
	form.Set("nume", "1")
	form.Set("type", mediaType)

	// Match Laravel's getEmbedUrl with Referer = pageUrl
	ajaxResp, err := doRequest("POST", ajaxEndpoint, pageURL, form.Encode())
	if err != nil {
		if accessErr := sourceAccessError(err); accessErr != nil {
			return "", "", "", accessErr
		}
		return "", "", "", err
	}

	var result embedResponse
	if err := json.Unmarshal([]byte(ajaxResp), &result); err != nil {
		return "", "", "", fmt.Errorf("failed to parse response: %w", err)
	}

	if strings.Contains(result.EmbedURL, "/error/") || result.Type == false {
		if mediaType == "tv" {
			return "", "", "", fmt.Errorf("this looks like a TV Show main page. Please send a link to a specific episode")
		}
		return "", "", "", fmt.Errorf("the website returned an error. Please ensure the URL is valid")
	}

	return result.EmbedURL, nextURL, displayTitle, nil
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
	stats := initDB()

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN not set")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}

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
		data := update.CallbackQuery.Data
		if data == "refresh" {
			handleRefresh(bot, update.CallbackQuery, stats)
		} else if data == "next" {
			handleNext(bot, update.CallbackQuery, stats)
		}
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)
	username := getUsername(msg.From)

	if text == "" {
		return
	}

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

	if !strings.HasPrefix(text, "http") {
		send(bot, chatID, "Please send a valid URL.")
		return
	}
	if !strings.Contains(text, "khdiamond.net") {
		send(bot, chatID, "Only khdiamond.net URLs are supported.")
		return
	}

	processRequest(bot, chatID, text, username, stats)
}

func handleRefresh(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, stats *Stats) {
	chatID := query.Message.Chat.ID
	username := getUsername(query.From)
	bot.Request(tgbotapi.NewCallback(query.ID, "Refreshing link..."))

	lastURL, _, lastMsgID, err := stats.getLastInfo(chatID)
	if err != nil || lastURL == "" {
		send(bot, chatID, "No previous link found to refresh.")
		return
	}

	if lastMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, lastMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		bot.Send(edit)
	}

	processRequest(bot, chatID, lastURL, username, stats)
}

func handleNext(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, stats *Stats) {
	chatID := query.Message.Chat.ID
	username := getUsername(query.From)
	bot.Request(tgbotapi.NewCallback(query.ID, "Loading next episode..."))

	_, nextURL, lastMsgID, err := stats.getLastInfo(chatID)
	if err != nil || nextURL == "" {
		send(bot, chatID, "No next episode found.")
		return
	}

	if lastMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, lastMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		bot.Send(edit)
	}

	processRequest(bot, chatID, nextURL, username, stats)
}

func processRequest(bot *tgbotapi.BotAPI, chatID int64, pageURL string, username string, stats *Stats) {
	_, _, oldMsgID, _ := stats.getLastInfo(chatID)
	if oldMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, oldMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		bot.Send(edit)
	}

	stats.trackUser(chatID, username, pageURL)
	loadingMsg, _ := bot.Send(tgbotapi.NewMessage(chatID, "Analyzing URL and fetching stream..."))

	embedURL, nextURL, displayTitle, err := getKhdiamondStream(pageURL)

	if err != nil {
		send(bot, chatID, fmt.Sprintf("Error: %s", err.Error()))
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
