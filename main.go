package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type ScrapeJob struct {
	ID          string
	URL         string
	Depth       int
	MaxPages    int
	Status      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	Results     []ScrapedPage
}

type ScrapedPage struct {
	URL       string
	Title     string
	Links     []string
	Images    []string
	ScrapedAt time.Time
}

type URLDepth struct {
	URL   string
	Depth int
}

type Storage interface {
	SaveJob(job *ScrapeJob) error
	GetJob(id string) (*ScrapeJob, error)
	ListJobs() ([]*ScrapeJob, error)
}

type MemoryStorage struct {
	mu   sync.RWMutex
	jobs map[string]*ScrapeJob
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		jobs: make(map[string]*ScrapeJob),
	}
}

func (m *MemoryStorage) SaveJob(job *ScrapeJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = job
	return nil
}

func (m *MemoryStorage) GetJob(id string) (*ScrapeJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, exists := m.jobs[id]
	if !exists {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return job, nil
}

func (m *MemoryStorage) ListJobs() ([]*ScrapeJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]*ScrapeJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	return jobs, nil
}

type Scraper struct {
	visited     sync.Map
	rateLimiter *rate.Limiter
	storage     Storage
}

func NewScraper(requestsPerSecond float64, storage Storage) *Scraper {
	return &Scraper{
		rateLimiter: rate.NewLimiter(rate.Limit(requestsPerSecond), 1),
		storage:     storage,
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
		URL:       url,
		Title:     doc.Find("title").Text(),
		ScrapedAt: time.Now(),
		Links:     []string{},
		Images:    []string{},
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
		if _, visited := s.visited.LoadOrStore(current.URL, true); visited {
			continue
		}

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
				toVisit = append(toVisit, URLDepth{
					URL:   link,
					Depth: current.Depth + 1,
				})
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
		ID:        uuid.New().String(),
		URL:       url,
		Depth:     depth,
		MaxPages:  maxPages,
		Status:    "pending",
		CreatedAt: time.Now(),
		Results:   []ScrapedPage{},
	}
}

func main() {
	storage := NewMemoryStorage()
	scraper := NewScraper(2.0, storage)

	job := NewScrapeJob("https://example.com", 2, 10)
	fmt.Printf("Created job %s\n", job.ID)

	if err := scraper.ProcessJob(job); err != nil {
		log.Fatal(err)
	}

	// Retrieve from storage
	savedJob, err := storage.GetJob(job.ID)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Job %s: scraped %d pages\n", savedJob.ID, len(savedJob.Results))
}
