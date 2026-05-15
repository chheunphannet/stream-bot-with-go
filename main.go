package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	_ "github.com/glebarez/go-sqlite"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	dbFile       = "stats.db"
	ajaxEndpoint = "https://khdiamond.net/wp-admin/admin-ajax.php"
	baseReferer  = "https://khdiamond.net/"
)

var defaultHeaders = map[string]string{
	"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	"Accept-Language":           "en-US,en;q=0.9,km;q=0.8",
	"Accept-Encoding":           "gzip, deflate, br",
	"Cache-Control":             "max-age=0",
	"Connection":                "keep-alive",
	"Sec-Ch-Ua":                 "\"Not_A Brand\";v=\"8\", \"Chromium\";v=\"120\", \"Google Chrome\";v=\"120\"",
	"Sec-Ch-Ua-Mobile":          "?0",
	"Sec-Ch-Ua-Platform":        "\"Windows\"",
	"Sec-Fetch-Dest":            "document",
	"Sec-Fetch-Mode":            "navigate",
	"Sec-Fetch-Site":            "none",
	"Sec-Fetch-User":            "?1",
	"Upgrade-Insecure-Requests": "1",
}

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

// ── HTTP Client (Cloudflare Bypass) ────────────────────────────────────────

var httpClient tls_client.HttpClient

func init() {
	options := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_120),
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithCookieJar(tls_client.NewCookieJar()), // Enable cookie support
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		log.Fatal("Failed to create TLS client:", err)
	}
	httpClient = client
}

func doRequest(method, targetURL, referer string, body io.Reader) (string, error) {
	req, err := fhttp.NewRequest(method, targetURL, body)
	if err != nil {
		return "", err
	}

	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://khdiamond.net")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 {
		return "", fmt.Errorf("Cloudflare is blocking your Singapore VPS IP (403 Forbidden). Please move to a Cambodia VPS or use a Proxy")
	}

	if resp.StatusCode != fhttp.StatusOK {
		return "", fmt.Errorf("failed to fetch %s (%d)", targetURL, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

func fetchHTML(pageURL, referer string) (string, error) {
	return doRequest("GET", pageURL, referer, nil)
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
	html, err := fetchHTML(pageURL, "")
	if err != nil {
		return "", "", "", err
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
		return "", "", "", fmt.Errorf("no post ID found on page")
	}
	postID := matches[1]

	form := url.Values{}
	form.Set("action", "doo_player_ajax")
	form.Set("post", postID)
	form.Set("nume", "1")
	form.Set("type", mediaType)

	ajaxResp, err := doRequest("POST", ajaxEndpoint, pageURL, strings.NewReader(form.Encode()))
	if err != nil {
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
