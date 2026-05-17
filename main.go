package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/glebarez/go-sqlite"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	dbFile               = "stats.db"
	ajaxEndpoint         = "https://khdiamond.net/wp-admin/admin-ajax.php"
	defaultUpdateWorkers = 8
	defaultFetchWorkers  = 4
	requestTimeout       = 90 * time.Second
)

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		log.Printf("Invalid %s=%q; using %d", key, value, fallback)
		return fallback
	}

	return n
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

	dbMaxOpenConns := envInt("DB_MAX_OPEN_CONNS", 4)
	db.SetMaxOpenConns(dbMaxOpenConns)
	db.SetMaxIdleConns(dbMaxOpenConns)

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
	if _, err := s.db.Exec(query, msgID, fmt.Sprintf("%d", chatID)); err != nil {
		log.Println("Database error (updateLastMessage):", err)
	}
}

func (s *Stats) updateNextURL(chatID int64, nextURL string) {
	query := "UPDATE users SET next_url = ? WHERE id = ?"
	if _, err := s.db.Exec(query, nextURL, fmt.Sprintf("%d", chatID)); err != nil {
		log.Println("Database error (updateNextURL):", err)
	}
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
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(request_count), 0) FROM users").Scan(&totalUsers, &totalRequests); err != nil {
		log.Println("Database error (report totals):", err)
		return "Error generating report"
	}

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
		if err := rows.Scan(&uname, &count); err != nil {
			log.Println("Database error (report row):", err)
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. @%s — %d requests", i, uname, count))
		i++
	}
	if err := rows.Err(); err != nil {
		log.Println("Database error (report rows):", err)
		return "Error generating report"
	}

	return fmt.Sprintf(
		"Stats:\n\nTotal Users: %d\nTotal Requests: %d\n\nTop 5 Users:\n%s",
		totalUsers,
		totalRequests,
		strings.Join(lines, "\n"),
	)
}

// ── HTTP Client (curl_chrome120 / curl-impersonate) ────────────────────────

func doRequest(ctx context.Context, method, targetURL, referer, bodyStr string) (string, error) {
	args := []string{
		"-sS", "-L", "-k", "--compressed",
		"--connect-timeout", "10",
		"--max-time", "75",
		"-X", method,
		"-H", "Authority: khdiamond.net",
		"-H", "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"-H", "Accept-Language: en-US,en;q=0.9",
		"-H", "Cache-Control: max-age=0",
		"-H", "Sec-Ch-Ua: \"Not A(Brand\";v=\"99\", \"Google Chrome\";v=\"120\", \"Chromium\";v=\"120\"",
		"-H", "Sec-Ch-Ua-Mobile: ?0",
		"-H", "Sec-Ch-Ua-Platform: \"Windows\"",
		"-H", "Sec-Fetch-Dest: document",
		"-H", "Sec-Fetch-Mode: navigate",
		"-H", "Sec-Fetch-Site: none",
		"-H", "Sec-Fetch-User: ?1",
		"-H", "Upgrade-Insecure-Requests: 1",
		"-H", "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}

	if referer != "" {
		args = append(args, "-H", "Referer: "+referer)
	}

	if method == "POST" {
		args = append(args, "-H", "Content-Type: application/x-www-form-urlencoded")
		args = append(args, "-H", "X-Requested-With: XMLHttpRequest")
		args = append(args, "-d", bodyStr)
	}

	args = append(args, targetURL)

	cmd := exec.CommandContext(ctx, "curl_chrome120", args...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("request timed out after %s", requestTimeout)
		}
		return "", fmt.Errorf("curl_chrome120 error: %v, stderr: %s", err, stderr.String())
	}

	resp := out.String()
	if strings.Contains(resp, "Just a moment...") || strings.Contains(resp, "cf-challenge") {
		return resp, fmt.Errorf("Cloudflare JS challenge detected. curl_chrome120 alone cannot solve this")
	}

	return resp, nil
}

func fetchHTML(ctx context.Context, pageURL, referer string) (string, error) {
	return doRequest(ctx, "GET", pageURL, referer, "")
}

// ── Stream Extraction ──────────────────────────────────────────────────────

var (
	postIDRegex    = regexp.MustCompile(`postid-(\d+)`)
	shortlinkRegex = regexp.MustCompile(`\?p=(\d+)`)
	nextURLRegex   = regexp.MustCompile(`<a href=['"]([^'"]+)['"][^>]*>\s*<span>ភាគបន្ទាប់</span>`)
	titleRegex     = regexp.MustCompile(`<h1[^>]*>([^<]+)</h1>`)
	numRegex       = regexp.MustCompile(`(\d+)x(\d+)|S(\d+)\s*-\s*E(\d+)`)
	cleanupRegex   = regexp.MustCompile(`[:\-\s]+$`)
)

type embedResponse struct {
	EmbedURL string `json:"embed_url"`
	Type     any    `json:"type"`
}

func getKhdiamondStream(ctx context.Context, pageURL string) (string, string, string, error) {
	u, err := url.Parse(pageURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("invalid page URL")
	}
	pageHTML, err := fetchHTML(ctx, pageURL, fmt.Sprintf("%s://%s", u.Scheme, u.Host))
	if err != nil {
		return "", "", "", err
	}

	mediaType := "movie"
	if strings.Contains(pageHTML, "single-episodes") || strings.Contains(pageHTML, "single-tvshows") || strings.Contains(pageURL, "/episodes/") || strings.Contains(pageURL, "/tvshows/") {
		mediaType = "tv"
	}

	title := "Unknown"
	titleMatches := titleRegex.FindStringSubmatch(pageHTML)
	if len(titleMatches) > 1 {
		title = html.UnescapeString(strings.TrimSpace(titleMatches[1]))
	}

	nextURL := ""
	if mediaType == "tv" {
		nextMatches := nextURLRegex.FindStringSubmatch(pageHTML)
		if len(nextMatches) > 1 {
			nextURL = nextMatches[1]
		}
	}

	metaInfo := ""
	if mediaType == "tv" {
		numMatches := numRegex.FindStringSubmatch(title)
		if len(numMatches) == 0 {
			numMatches = numRegex.FindStringSubmatch(pageHTML)
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

	// Try multiple ways to find the Post ID
	postID := ""
	if m := postIDRegex.FindStringSubmatch(pageHTML); len(m) > 1 {
		postID = m[1]
	} else if m := shortlinkRegex.FindStringSubmatch(pageHTML); len(m) > 1 {
		postID = m[1]
	}

	if postID == "" {
		// Log a snippet of HTML for debugging
		snippet := pageHTML
		if len(snippet) > 1000 {
			snippet = snippet[:1000]
		}
		log.Printf("DEBUG: Failed to find post ID. URL: %s HTML Snippet: %s", pageURL, snippet)
		return "", "", "", fmt.Errorf("no post ID found on page. (Site might have changed structure)")
	}

	form := url.Values{}
	form.Set("action", "doo_player_ajax")
	form.Set("post", postID)
	form.Set("nume", "1")
	form.Set("type", mediaType)

	ajaxResp, err := doRequest(ctx, "POST", ajaxEndpoint, pageURL, form.Encode())
	if err != nil {
		return "", "", "", err
	}

	cleanResp := ajaxResp
	if strings.Contains(ajaxResp, "<pre>") {
		start := strings.Index(ajaxResp, "<pre>") + 5
		end := strings.LastIndex(ajaxResp, "</pre>")
		if end > start {
			cleanResp = ajaxResp[start:end]
		}
	}

	var result embedResponse
	if err := json.Unmarshal([]byte(cleanResp), &result); err != nil {
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

func isKhdiamondURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}

	host := strings.ToLower(parsed.Hostname())
	return host == "khdiamond.net" || strings.HasSuffix(host, ".khdiamond.net")
}

type BotServer struct {
	bot          *tgbotapi.BotAPI
	stats        *Stats
	fetchLimiter chan struct{}
	chatLocks    sync.Map
}

func newBotServer(bot *tgbotapi.BotAPI, stats *Stats, maxFetches int) *BotServer {
	return &BotServer{
		bot:          bot,
		stats:        stats,
		fetchLimiter: make(chan struct{}, maxFetches),
	}
}

func (s *BotServer) chatLock(chatID int64) *sync.Mutex {
	lock, _ := s.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *BotServer) withChatLock(chatID int64, fn func()) {
	lock := s.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()
	fn()
}

func (s *BotServer) answerCallback(query *tgbotapi.CallbackQuery, text string) {
	if query == nil {
		return
	}
	if _, err := s.bot.Request(tgbotapi.NewCallback(query.ID, text)); err != nil {
		log.Printf("Telegram callback error: %v", err)
	}
}

func main() {
	stats := initDB()
	defer stats.db.Close()

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

	server := newBotServer(bot, stats, envInt("FETCH_WORKERS", defaultFetchWorkers))
	updateWorkers := envInt("UPDATE_WORKERS", defaultUpdateWorkers)
	log.Printf("Worker limits: updates=%d fetches=%d", updateWorkers, cap(server.fetchLimiter))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for i := 0; i < updateWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case update, ok := <-updates:
					if !ok {
						return
					}
					server.handleUpdate(update)
				}
			}
		}()
	}

	<-ctx.Done()
	log.Println("Shutdown requested. Stopping Telegram updates...")
	bot.StopReceivingUpdates()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("Shutdown complete.")
	case <-time.After(15 * time.Second):
		log.Println("Shutdown timeout reached; exiting with active handlers.")
	}
}

func (s *BotServer) handleUpdate(update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		query := update.CallbackQuery
		if query.Message == nil {
			s.answerCallback(query, "Message is no longer available.")
			return
		}

		switch query.Data {
		case "refresh":
			s.answerCallback(query, "Refreshing link...")
			s.withChatLock(query.Message.Chat.ID, func() {
				s.handleRefresh(query)
			})
		case "next":
			s.answerCallback(query, "Loading next episode...")
			s.withChatLock(query.Message.Chat.ID, func() {
				s.handleNext(query)
			})
		default:
			s.answerCallback(query, "Unknown action.")
		}
		return
	}

	if update.Message == nil {
		return
	}

	s.withChatLock(update.Message.Chat.ID, func() {
		s.handleMessage(update.Message)
	})
}

func (s *BotServer) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)
	username := getUsername(msg.From)

	if text == "" {
		return
	}

	if text == "/start" {
		send(s.bot, chatID, "Send me a khdiamond.net URL and I will get the stream for you.")
		return
	}

	if text == "/count_process" {
		send(s.bot, chatID, s.stats.report())
		return
	}

	if strings.HasPrefix(text, "/") {
		return
	}

	if !strings.HasPrefix(strings.ToLower(text), "http") {
		send(s.bot, chatID, "Please send a valid URL.")
		return
	}
	if !isKhdiamondURL(text) {
		send(s.bot, chatID, "Only khdiamond.net URLs are supported.")
		return
	}

	s.processRequest(chatID, text, username)
}

func (s *BotServer) handleRefresh(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	username := getUsername(query.From)

	lastURL, _, lastMsgID, err := s.stats.getLastInfo(chatID)
	if err != nil || lastURL == "" {
		send(s.bot, chatID, "No previous link found to refresh.")
		return
	}

	if lastMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, lastMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		if _, err := s.bot.Send(edit); err != nil {
			log.Printf("Telegram edit error: %v", err)
		}
	}

	s.processRequest(chatID, lastURL, username)
}

func (s *BotServer) handleNext(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	username := getUsername(query.From)

	_, nextURL, lastMsgID, err := s.stats.getLastInfo(chatID)
	if err != nil || nextURL == "" {
		send(s.bot, chatID, "No next episode found.")
		return
	}

	if lastMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, lastMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		if _, err := s.bot.Send(edit); err != nil {
			log.Printf("Telegram edit error: %v", err)
		}
	}

	s.processRequest(chatID, nextURL, username)
}

func (s *BotServer) processRequest(chatID int64, pageURL string, username string) {
	_, _, oldMsgID, _ := s.stats.getLastInfo(chatID)
	if oldMsgID != 0 {
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, oldMsgID, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
		if _, err := s.bot.Send(edit); err != nil {
			log.Printf("Telegram edit error: %v", err)
		}
	}

	s.stats.trackUser(chatID, username, pageURL)
	loadingMsg, err := s.bot.Send(tgbotapi.NewMessage(chatID, "Analyzing URL and fetching stream..."))
	if err != nil {
		log.Printf("Telegram send loading error: %v", err)
	}

	s.fetchLimiter <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	embedURL, nextURL, displayTitle, err := getKhdiamondStream(ctx, pageURL)
	cancel()
	<-s.fetchLimiter

	if err != nil {
		send(s.bot, chatID, fmt.Sprintf("Error: %s", err.Error()))
	} else {
		s.stats.updateNextURL(chatID, nextURL)
		msgText := fmt.Sprintf("%s\n\nEmbed URL:\n%s", displayTitle, embedURL)
		msg := tgbotapi.NewMessage(chatID, msgText)

		var row []tgbotapi.InlineKeyboardButton
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh Link", "refresh"))
		if nextURL != "" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("⏭ Next Episode", "next"))
		}

		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(row...))
		sentMsg, err := s.bot.Send(msg)
		if err != nil {
			log.Printf("Telegram send result error: %v", err)
		} else {
			s.stats.updateLastMessage(chatID, sentMsg.MessageID)
		}
	}

	if loadingMsg.MessageID != 0 {
		if _, err := s.bot.Request(tgbotapi.NewDeleteMessage(chatID, loadingMsg.MessageID)); err != nil {
			log.Printf("Telegram delete loading error: %v", err)
		}
	}
}

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	if _, err := bot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("Telegram send error: %v", err)
	}
}
