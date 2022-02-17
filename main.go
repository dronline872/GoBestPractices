package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type CrawlResult struct {
	Err   error
	Title string
	Url   string
}

type Page interface {
	GetTitle() string
	GetLinks() []string
}

type page struct {
	doc *goquery.Document
}

func NewPage(raw io.Reader) (*page, error) {
	doc, err := goquery.NewDocumentFromReader(raw)
	if err != nil {
		return nil, err
	}
	return &page{doc: doc}, nil
}

func (p *page) GetTitle() string {
	return p.doc.Find("title").First().Text()
}

func (p *page) GetLinks() []string {
	var urls []string
	p.doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		url, ok := s.Attr("href")
		if ok {
			urls = append(urls, url)
		}
	})
	return urls
}

type Requester interface {
	Get(ctx context.Context, url string) (Page, error)
}

type requester struct {
	timeout time.Duration
}

func NewRequester(timeout time.Duration) requester {
	return requester{timeout: timeout}
}

func (r requester) Get(ctx context.Context, url string) (Page, error) {
	select {
	case <-ctx.Done():
		return nil, nil
	default:
		cl := &http.Client{
			Timeout: r.timeout,
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		body, err := cl.Do(req)
		if err != nil {
			return nil, err
		}
		defer body.Body.Close()
		page, err := NewPage(body.Body)
		if err != nil {
			return nil, err
		}
		return page, nil
	}

}

//Crawler - интерфейс (контракт) краулера
type Crawler interface {
	Scan(ctx context.Context, url string, depth int, domain string)
	ChanResult() <-chan CrawlResult
	AddDepth()
}

type crawler struct {
	r       Requester
	res     chan CrawlResult
	visited map[string]struct{}
	mu      sync.RWMutex
	depth   int
}

func NewCrawler(r Requester) *crawler {
	return &crawler{
		r:       r,
		res:     make(chan CrawlResult),
		visited: make(map[string]struct{}),
		mu:      sync.RWMutex{},
	}
}

func (c *crawler) Scan(ctx context.Context, url string, depth int, domain string) {
	if c.depth > 0 {
		depth += c.depth
		c.mu.Lock()
		c.depth = 0
		c.mu.Unlock()
	}

	if depth <= 0 { //Проверяем то, что есть запас по глубине
		return
	}
	c.mu.RLock()
	_, ok := c.visited[url] //Проверяем, что мы ещё не смотрели эту страницу
	c.mu.RUnlock()
	if ok {
		return
	}
	select {
	case <-ctx.Done(): //Если контекст завершен - прекращаем выполнение
		return
	default:
		page, err := c.r.Get(ctx, url) //Запрашиваем страницу через Requester
		if err != nil {
			c.res <- CrawlResult{Err: err} //Записываем ошибку в канал
			return
		}
		c.mu.Lock()
		c.visited[url] = struct{}{} //Помечаем страницу просмотренной
		c.mu.Unlock()
		c.res <- CrawlResult{ //Отправляем результаты в канал
			Title: page.GetTitle(),
			Url:   url,
		}
		for _, link := range page.GetLinks() {
			hasHttp := strings.Contains(link, "http")
			if !hasHttp {
				link = domain + link
			}

			go c.Scan(ctx, link, depth-1, domain) //На все полученные ссылки запускаем новую рутину сборки
		}
	}
}

func (c *crawler) ChanResult() <-chan CrawlResult {
	return c.res
}

func (c *crawler) AddDepth() {
	c.mu.Lock()
	c.depth += 2
	c.mu.Unlock()
}

//Config - структура для конфигурации
type Config struct {
	MaxDepth   int
	MaxResults int
	MaxErrors  int
	Url        string
	Timeout    int //in seconds
}

func main() {

	cfg := Config{
		MaxDepth:   3,
		MaxResults: 10,
		MaxErrors:  5,
		Url:        "https://telegram.org",
		Timeout:    20,
	}
	var cr Crawler
	var r Requester

	r = NewRequester(time.Duration(cfg.Timeout) * time.Second)
	cr = NewCrawler(r)

	ctx, cancel := context.WithCancel(context.Background())
	go cr.Scan(ctx, cfg.Url, cfg.MaxDepth, cfg.Url) //Запускаем краулер в отдельной рутине
	go processResult(ctx, cancel, cr, cfg)          //Обрабатываем результаты в отдельной рутине

	sigCh := make(chan os.Signal, 1) //Создаем канал для приема сигналов
	signal.Notify(sigCh,
		syscall.SIGINT,
		syscall.SIGUSR1,
	) //Подписываемся на сигнал SIGINT
	for {
		select {

		case <-ctx.Done(): //Если всё завершили - выходим
			return
		case signal := <-sigCh:
			if signal == syscall.SIGUSR1 {
				cr.AddDepth()
			} else if signal == syscall.SIGINT {
				cfg.MaxDepth = cfg.MaxDepth + 50
				cancel() //Если пришёл сигнал SigInt - завершаем контекст
			}

		}
	}
}

func processResult(ctx context.Context, cancel func(), cr Crawler, cfg Config) {
	var maxResult, maxErrors = cfg.MaxResults, cfg.MaxErrors
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-cr.ChanResult():
			if msg.Err != nil {
				maxErrors--
				log.Printf("crawler result return err: %s\n", msg.Err.Error())
				if maxErrors <= 0 {
					cancel()
					return
				}
			} else {
				maxResult--
				log.Printf("crawler result: [url: %s] Title: %s\n", msg.Url, msg.Title)
				if maxResult <= 0 {
					cancel()
					return
				}
			}
		}
	}
}
