package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/joho/godotenv"
	"google.golang.org/genai"
)

const StateFile = "state.json"

// サイトの構造定義
type SiteConfig struct {
	RootSelector  string `json:"root"`
	TitleSelector string `json:"title"`
	ShopSelector  string `json:"shop"`
	PriceSelector string `json:"price"`
	LinkSelector  string `json:"link"`
	ImageSelector string `json:"image"`
}

// 監視対象リスト
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type State map[string]int
type Item struct {
	ID       int
	Title    string
	Price    string
	ShopName string
	ImageURL string
	PageURL  string
}

// Discord Webhook構造体（Username追加）
type DiscordWebhook struct {
	Username string  `json:"username,omitempty"`
	Embeds   []Embed `json:"embeds"`
}
type Embed struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Color       int       `json:"color"`
	Thumbnail   *EmbedImg `json:"thumbnail,omitempty"`
	Footer      *Footer   `json:"footer,omitempty"`
}
type EmbedImg struct {
	URL string `json:"url"`
}
type Footer struct {
	Text string `json:"text"`
}

func main() {
	_ = godotenv.Load()
	webhookURL := strings.Split(os.Getenv("DISCORD_WEBHOOK_URL"), ",")
	log.Println(os.Getenv("DISCORD_WEBHOOK_URL"))

	configEnv := os.Getenv("SITE_CONFIG_JSON")
	if configEnv == "" {
		log.Fatal("Error: SITE_CONFIG_JSON is not set")
	}
	var siteConfig SiteConfig
	if err := json.Unmarshal([]byte(configEnv), &siteConfig); err != nil {
		log.Fatalf("SITE_CONFIG_JSON parse error: %v", err)
	}

	targetsEnv := os.Getenv("TARGETS_JSON")
	if targetsEnv == "" {
		log.Fatal("Error: TARGETS_JSON is not set")
	}
	var targets []Target
	if err := json.Unmarshal([]byte(targetsEnv), &targets); err != nil {
		log.Fatalf("TARGETS_JSON parse error: %v", err)
	}

	state, err := loadState()
	if err != nil {
		state = make(State)
	}

	// 5分のタイムアウトを設定
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	var stateMutex sync.Mutex
	anyUpdated := false

	log.Println("Starting checks with 5 minute timeout...")

	// ターゲットごとにGoroutineを起動
	for _, target := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()

			// 処理開始前にContextチェック
			if ctx.Err() != nil {
				return
			}

			log.Printf("[%s] Checking...", t.Name)

			// スクレイピング実行
			items, err := scrapeGeneric(ctx, t.URL, siteConfig)
			if err != nil {
				log.Printf("[%s] Error: %v", t.Name, err)
				return
			}

			// 現在のLastIDを取得（排他制御）
			stateMutex.Lock()
			lastID := state[t.Name]
			stateMutex.Unlock()

			newLastID := lastID
			foundNew := false

			// アイテムを古い順（ID昇順）またはリストの後ろから処理
			// 元のロジックに合わせてリストの後ろから処理
			for i := len(items) - 1; i >= 0; i-- {
				// ループごとにContextチェック
				select {
				case <-ctx.Done():
					log.Printf("[%s] Time limit reached. Stopping...", t.Name)
					goto FINISH
				default:
				}

				item := items[i]
				if item.ID > lastID {
					log.Printf("[%s] New item: %s", t.Name, item.Title)

					// Geminiで説明文を生成
					mode := os.Getenv("MODE")
					SECRET := os.Getenv("SECRET")
					var description string
					if mode == SECRET {
						description = generateGeminiDescription(ctx, item)
					} else {
						description = "This mode is testing mode. because you dont have special secret ke.y"
					}

					// Discord送信
					sendDiscordEmbed(webhookURL, item, t.Name, description)

					if item.ID > newLastID {
						newLastID = item.ID
					}
					foundNew = true

					// APIレート制限などを考慮して少し待機（Context対応）
					select {
					case <-ctx.Done():
						goto FINISH
					case <-time.After(1 * time.Second):
					}
				}
			}

		FINISH:
			// 更新があればStateを更新（排他制御）
			if foundNew {
				stateMutex.Lock()
				// 他のGoroutineが更新している可能性も考慮し、より大きいIDを採用
				if newLastID > state[t.Name] {
					state[t.Name] = newLastID
					anyUpdated = true
				}
				stateMutex.Unlock()
			}
		}(target)
	}

	// 全Goroutineの終了を待機
	wg.Wait()

	if ctx.Err() == context.DeadlineExceeded {
		log.Println("Global time limit reached.")
	}

	if anyUpdated {
		saveState(state)
		log.Println("State updated.")
	} else {
		log.Println("No updates found.")
	}
}

// scrapeGeneric: Contextを受け取るように変更
func scrapeGeneric(ctx context.Context, url string, config SiteConfig) ([]Item, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, err
	}

	var items []Item

	doc.Find(config.RootSelector).Each(func(_ int, s *goquery.Selection) {
		// Contextがキャンセルされていたら処理を中断したいが、
		// goqueryのEachは中断できないため、ループ内でチェックしても効果は薄い。
		// ただし、重い処理がある場合はここでチェックする。

		idStr, _ := s.Attr("data-product-id")
		id, _ := strconv.Atoi(idStr)
		if id == 0 {
			return
		}

		title := strings.TrimSpace(s.Find(config.TitleSelector).Text())
		shop := strings.TrimSpace(s.Find(config.ShopSelector).Text())
		price := strings.TrimSpace(s.Find(config.PriceSelector).Text())
		link, _ := s.Find(config.LinkSelector).Attr("href")

		imageUrl, _ := s.Find(config.ImageSelector).Attr("style")
		reg := regexp.MustCompile(`https?://[^\s/$.?#].\S*\.jpg`)
		imageUrl = reg.FindString(imageUrl)

		items = append(items, Item{
			ID: id, Title: title, Price: price, ShopName: shop, ImageURL: imageUrl, PageURL: link,
		})
	})
	return items, nil
}

// generateGeminiDescription: Contextを受け取るように変更
func generateGeminiDescription(ctx context.Context, item Item) string {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return "Gemini API Key is not set."
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		log.Printf("Failed to create Gemini client: %v", err)
		return "Failed to initialize AI."
	}
	promptBaseText := os.Getenv("PROMPT_BASE_TEXT")
	prompt := fmt.Sprintf(promptBaseText+"\n\n商品名: %s\n価格: %s\nショップ: %s\nリンク: %s", item.Title, item.Price, item.ShopName, item.PageURL)

	// gemini-2.5-flash を使用
	resp, err := client.Models.GenerateContent(ctx, "gemini-2.5-flash", genai.Text(prompt), nil)
	if err != nil {
		log.Printf("Gemini API error: %v", err)
		return "AI description unavailable."
	}

	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		return fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0].Text)
	}

	return "No description generated."
}

func loadState() (State, error) {
	s := make(State)
	f, err := os.ReadFile(StateFile)
	if err != nil {
		return s, nil
	}
	return s, json.Unmarshal(f, &s)
}

func saveState(s State) error {
	d, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(StateFile, d, 0644)
}

func sendDiscordEmbed(webhookURL []string, item Item, src string, aiDescription string) {
	description := fmt.Sprintf("**Price:** %s\n**Shop:** %s\n\n**紹介:**\n%s", item.Price, item.ShopName, aiDescription)

	payload := DiscordWebhook{
		Username: src, // 通知名をTargetのNameにする
		Embeds: []Embed{{
			Title: item.Title, Description: description,
			URL: item.PageURL, Color: 0xFC4D50, Thumbnail: &EmbedImg{URL: item.ImageURL},
			Footer: &Footer{Text: fmt.Sprintf("[%s] ID: %d", src, item.ID)},
		}},
	}
	jsonPayload, _ := json.Marshal(payload)
	for _, wURL := range webhookURL {
		http.Post(wURL, "application/json", bytes.NewBuffer(jsonPayload))
	}
}
