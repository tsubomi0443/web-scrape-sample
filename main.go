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
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/joho/godotenv"
	"google.golang.org/genai"
)

const StateFile = "state.json"

// 【変更点1】サイトの構造定義（セレクタのみ）
type SiteConfig struct {
	RootSelector  string `json:"root"`
	TitleSelector string `json:"title"`
	ShopSelector  string `json:"shop"`
	PriceSelector string `json:"price"`
	LinkSelector  string `json:"link"`
	ImageSelector string `json:"image"`
}

// 【変更点2】監視対象リスト（名前とURLのみ）
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// 以下の構造体は変更なし
type State map[string]int
type Item struct {
	ID       int
	Title    string
	Price    string
	ShopName string
	ImageURL string
	PageURL  string
}
type DiscordWebhook struct {
	Embeds []Embed `json:"embeds"`
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
	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL == "" {
		log.Fatal("Error: DISCORD_WEBHOOK_URL is not set")
	}

	// 1. サイト設定（セレクタ）の読み込み
	configEnv := os.Getenv("SITE_CONFIG_JSON")
	if configEnv == "" {
		log.Fatal("Error: SITE_CONFIG_JSON is not set")
	}
	var siteConfig SiteConfig
	if err := json.Unmarshal([]byte(configEnv), &siteConfig); err != nil {
		log.Fatalf("SITE_CONFIG_JSON parse error: %v", err)
	}

	// 2. ターゲットリスト（URL）の読み込み
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

	updated := false
	startTime := time.Now()
	const timeLimit = 5 * time.Minute

	// ターゲットごとにループ
	for _, target := range targets {
		if time.Since(startTime) > timeLimit {
			log.Println("Time limit reached. Stopping...")
			break
		}

		log.Printf("Checking: %s", target.Name)

		// URL(target) と 設定(siteConfig) を渡す
		items, err := scrapeGeneric(target.URL, siteConfig)
		if err != nil {
			log.Printf("  Error: %v", err)
			continue
		}

		lastID := state[target.Name]
		newLastID := lastID
		foundNew := false
		timeUp := false

		for i := len(items) - 1; i >= 0; i-- {
			if time.Since(startTime) > timeLimit {
				log.Println("Time limit reached during item processing. Stopping...")
				timeUp = true
				break
			}

			item := items[i]
			if item.ID > lastID {
				log.Printf("  New item: %s", item.Title)

				// Geminiで説明文を生成
				aiDescription := generateGeminiDescription(item)

				sendDiscordEmbed(webhookURL, item, target.Name, aiDescription)

				if item.ID > newLastID {
					newLastID = item.ID
				}
				foundNew = true
				time.Sleep(5 * time.Second)
			}
		}

		if foundNew {
			state[target.Name] = newLastID
			updated = true
		}

		if timeUp {
			break
		}

		time.Sleep(3 * time.Second)
	}

	if updated {
		saveState(state)
		log.Println("State updated.")
	}
}

// 【変更点3】引数を分離
func scrapeGeneric(url string, config SiteConfig) ([]Item, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

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

	// config内のセレクタを使用
	doc.Find(config.RootSelector).Each(func(_ int, s *goquery.Selection) {

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

// Gemini APIを使用して商品の説明文を生成する
func generateGeminiDescription(item Item) string {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return "Gemini API Key is not set."
	}

	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		log.Printf("Failed to create Gemini client: %v", err)
		return "Failed to initialize AI."
	}

	//prompt := fmt.Sprintf("以下の商品について、リンク先の内容を想像しつつ、フレンドリーかつ詳細に説明する紹介文を日本語で生成してください。文字列の長さは150文字程度でまとめてください。紹介文とは別に対応アバターが明記だれている場合に限り紹介文の上に箇条書で記載してください。明記されていなければ非表示にしてください。\n\n商品名: %s\n価格: %s\nショップ: %s\nリンク: %s",
	prompt := fmt.Sprintf("以下の商品について、リンク先の内容を想像しつつ、なんJ民風にレスを５レスぐらいの規模で詳細説明する紹介文を日本語で生成してください。とてもユーモアが必要な作業です。レスの長さは40文字程度でまとめてください。紹介文とは別に対応アバターが明記だれている場合に限り紹介文の上に箇条書で記載してください。明記されていなければ非表示にしてください。\n\n商品名: %s\n価格: %s\nショップ: %s\nリンク: %s",
		item.Title, item.Price, item.ShopName, item.PageURL)

	// gemini-2.5-flash を使用
	resp, err := client.Models.GenerateContent(ctx, "gemini-2.5-flash", genai.Text(prompt), nil)
	if err != nil {
		log.Printf("Gemini API error: %v", err)
		return "AI description unavailable."
	}

	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		//if text, ok := resp.Candidates[0].Content.Parts[0].Text.(string); ok {
		//	return text
		//}

		// Textがstringでない場合（構造体など）のフォールバックが必要なら記述するが、
		// genai.Text()でリクエストした場合、通常は文字列で返ってくる
		return fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0].Text)
	}

	return "No description generated."
}

// ヘルパー関数（変更なし）
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

func sendDiscordEmbed(webhookURL string, item Item, src string, aiDescription string) {
	description := fmt.Sprintf("**Price:** %s\n**Shop:** %s\n\n**AI紹介:**\n%s", item.Price, item.ShopName, aiDescription)

	payload := DiscordWebhook{Embeds: []Embed{{
		Title: item.Title, Description: description,
		URL: item.PageURL, Color: 0xFC4D50, Thumbnail: &EmbedImg{URL: item.ImageURL},
		Footer: &Footer{Text: fmt.Sprintf("[%s] ID: %d", src, item.ID)},
	}}}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonPayload))
}
