package main

import (
	"bytes"
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

	// ターゲットごとにループ
	for _, target := range targets {
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

		for i := len(items) - 1; i >= 0; i-- {
			item := items[i]
			if item.ID > lastID {
				log.Printf("  New item: %s", item.Title)
				sendDiscordEmbed(webhookURL, item, target.Name)

				if item.ID > newLastID {
					newLastID = item.ID
				}
				foundNew = true
				time.Sleep(2 * time.Second)
			}
		}

		if foundNew {
			state[target.Name] = newLastID
			updated = true
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

func sendDiscordEmbed(webhookURL string, item Item, src string) {
	payload := DiscordWebhook{Embeds: []Embed{{
		Title: item.Title, Description: fmt.Sprintf("**Price:** %s\n**Shop:** %s", item.Price, item.ShopName),
		URL: item.PageURL, Color: 0xFC4D50, Thumbnail: &EmbedImg{URL: item.ImageURL},
		Footer: &Footer{Text: fmt.Sprintf("[%s] ID: %d", src, item.ID)},
	}}}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonPayload))
}
