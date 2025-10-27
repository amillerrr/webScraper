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

type URLDepth struct {
	URL string
	Depth int
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
			page.Images = append(page.Images, src)
		}
	})

	return page, nil
}

func crawl(startURL string, maxDepth int, maxPages int) []ScrapedPage {
	results := []ScrapedPage{}

	// BFS queue from initial URL
	toVisit := []URLDepth{{URL: startURL, Depth: 0}}

	for len(toVisit) > 0 && len(results) < maxPages {
		current := toVisit[0]
		toVisit = toVisit[1:]

		// Scrape page
		page, err := scrapePage(current.URL)
		if err != nil {
			log.Printf("Error scraping %s: %v", current.URL, err)
			continue
		}

		results = append(results, page)
		fmt.Printf("Scraped: %s (depth %d)\n", current.URL, current.Depth)

		// Add child links if still not at max depth
		if current.Depth < maxDepth {
			for _, link := range page.Links {
				toVisit = append(toVisit, URLDepth{
					URL: link,
					Depth: current.Depth + 1,
				})
			}
		}
	}
	return results
}

func main() {
	results := crawl("https://example.com", 2, 10)
	fmt.Printf("\nTotal pages scraped: %d\n", len(results))
}
