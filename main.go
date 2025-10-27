package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type ScrapedPage struct {
	URL string
	Title string
	Links []string
	Images []string
	ScrapedAt time.Time
}

func scrapePage(url string) (ScrapedPage, error) {
	resp, err := http.Get(url)
	if err != nil {
		return ScrapedPage{}, err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return ScrapedPage{}, err
	}

	page := ScrapedPage{
		URL: url,
		Title: doc.Find("title").Text(),
		ScrapedAt: time.Now(),
		Links: []string{},
		Images: []string{},
	}

	// Extract Links
	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists {
			page.Links = append(page.Links, href)
		}
	})

	// Extract images
	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		if src, exists := s.Attr("src"); exists {
			page.Links = append(page.Links, src)
		}
	})

	return page, nil
}

func main() {
	page, err := scrapePage("https://example.com")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("URL: %s\n", page.URL)
	fmt.Printf("Title: %s\n", page.Title)
	fmt.Printf("Found %d links and %d images\n", len(page.Links), len(page.Images))
}
