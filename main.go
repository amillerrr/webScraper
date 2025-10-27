package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type ScrapeJob struct {
	ID string
	URL string
	Depth int
	MaxPages int
	Status string
	CreatedAt time.Time
	CompletedAt *time.Time
	Results []ScrapedPage
}

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

type Scraper struct {
	visited map[string]bool
	rateLimiter *rate.Limiter
}

func NewScraper(requestsPerSecond float64) *Scraper {
	return &Scraper{
		visited: make(map[string]bool),
		rateLimiter: rate.NewLimiter(rate.Limit(requestsPerSecond), 1),
	}
}

func (s *Scraper) scrapePage(url string) (ScrapedPage, error) {
	// rate limiter permission step
	if err := s.rateLimiter.Wait(context.Background()); err != nil {
		return ScrapedPage{}, err
	}

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

func (s *Scraper) ProcessJob(job *ScrapeJob) error {
	job.Status = "running"
	results := []ScrapedPage{}

	// BFS queue from initial URL
	toVisit := []URLDepth{{URL: job.URL, Depth: 0}}

	for len(toVisit) > 0 && len(results) < job.MaxPages {
		current := toVisit[0]
		toVisit = toVisit[1:]

		// Skip visted URLs
		if s.visited[current.URL] {
			continue
		}
		s.visited[current.URL] = true

		// Scrape page
		page, err := s.scrapePage(current.URL)
		if err != nil {
			log.Printf("Error scraping %s: %v", current.URL, err)
			continue
		}

		results = append(results, page)

		// Add child links if still not at max depth
		if current.Depth < job.Depth {
			for _, link := range page.Links {
				if !s.visited[link] {
					toVisit = append(toVisit, URLDepth{
						URL: link,
						Depth: current.Depth + 1,
					})
				}
			}
		}
	}
	
	job.Results = results
	job.Status = "completed"
	now := time.Now()
	job.CompletedAt = &now

	return nil
}

func NewScrapeJob(url string, depth int, maxPages int) *ScrapeJob {
	return &ScrapeJob{
		ID: uuid.New().String(),
		URL: url,
		Depth: depth,
		MaxPages: maxPages,
		Status: "pending",
		CreatedAt: time.Now(),
		Results: []ScrapedPage{},
	}
}

func main() {
	scraper := NewScraper(2.0)

	job := NewScrapeJob("https://example.com", 2, 10)
	fmt.Printf("Created job %s\n", job.ID)

	if err := scraper.ProcessJob(job); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Job %s completed in %v\n", job.ID, job.CompletedAt.Sub(job.CreatedAt))
	fmt.Printf("Scraped %d pages\n", len(job.Results))
}
