package searchIndex

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/box1bs/Saturday/configs"
	"github.com/box1bs/Saturday/internal/app/index/tree"
	"github.com/box1bs/Saturday/internal/app/web"
	"github.com/box1bs/Saturday/internal/model"
	"github.com/box1bs/Saturday/pkg/workerPool"
	"github.com/google/uuid"
)

type SearchIndex struct {
	indexRepos  model.Repository
	mu        	*sync.RWMutex
	stemmer   	model.Stemmer
	stopWords 	model.StopWords
	logger    	model.Logger
	root 		*tree.TreeNode
	UrlsCrawled int32
	AvgLen	 	float64
	quitCTX		context.Context
}

func NewSearchIndex(stemmer model.Stemmer, stopWords model.StopWords, l model.Logger, ir model.Repository, context context.Context) *SearchIndex {
	return &SearchIndex{
		mu: new(sync.RWMutex),
		stopWords: stopWords,
		stemmer: stemmer,
		root: tree.NewNode("/"),
		logger: l,
		quitCTX: context,
		indexRepos: ir,
	}
}

func (idx *SearchIndex) Index(config *configs.ConfigData) error {
	wp := workerPool.NewWorkerPool(config.WorkersCount, config.TasksCount)
    mp := new(sync.Map)
	idx.indexRepos.LoadVisitedUrls(mp)
	defer idx.indexRepos.SaveVisitedUrls(mp)
	var rl *web.RateLimiter
	ctx, cancel := context.WithTimeout(context.Background(), 90 * time.Second)
	defer cancel()
    for _, url := range config.BaseURLs {
		if config.Rate > 0 {
			rl = web.NewRateLimiter(config.Rate)
			defer rl.Shutdown()
		}
		node := tree.NewNode(url)
		idx.root.AddChild(node)
        spider := web.NewSpider(url, config.MaxDepth, config.MaxLinksInPage, mp, wp, config.OnlySameDomain, rl)
        spider.Pool.Submit(func() {
            spider.CrawlWithContext(ctx, cancel, url, idx, node, 0)
        })
    }
	wp.Wait()
	wp.Stop()
    return nil
}

type requestRanking struct {
	includesWords 	int
	relation 		float64
	tf_idf 			float64
	bm25 			float64
	//any ranking scores
}

func (idx *SearchIndex) updateAVGLen() {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var wordCount int
	docs, err := idx.indexRepos.GetAllDocuments()
	if err != nil {
		log.Println(err)
		return
	}
	for _, doc := range docs {
		wordCount += int(doc.GetFullSize())
	}

	idx.AvgLen = float64(wordCount) / float64(len(docs))
}

func (idx *SearchIndex) Search(query string) []*model.Document {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	
	if idx.AvgLen == 0 {
		idx.updateAVGLen()
	}
	
	rank := make(map[uuid.UUID]*requestRanking)

	words := idx.TokenizeAndStem(query)
	index := make(map[string]map[uuid.UUID]int)
	for _, word := range words {
		mp, err := idx.indexRepos.GetDocumentsByWord(word)
		if err != nil {
			log.Println(err)
			return nil
		}
		index[word] = mp
		for docID := range mp {
			if _, ok := rank[docID]; !ok {
				rank[docID] = &requestRanking{}
			}
		}
	}

	idx.fetchDocuments(words, rank, index)
	if len(rank) == 0 {
		return nil
	}
	
	result := make([]*model.Document, 0, 50)
	alreadyIncluded := make(map[uuid.UUID]struct{})
	for _, word := range words {
		lenght, err := idx.indexRepos.GetDocumentsCount()
		if err != nil {
			log.Println(err)
			return nil
		}
		idf := math.Log(float64(lenght) / float64(len(index[word]))) + 1.0
		
		for docID, freq := range index[word] {
			doc, err := idx.indexRepos.GetDocumentByID(docID)
			if err != nil {
				log.Println(err)
				continue
			}
			if doc == nil {
				continue
			}
			
			rank[docID].tf_idf += float64(freq) / doc.GetFullSize() * (idf - 1.0)
			rank[docID].bm25 += culcBM25(idf, float64(freq), doc, idx.AvgLen)

			if _, ex := alreadyIncluded[docID]; ex {
				continue
			}
			alreadyIncluded[docID] = struct{}{}
			result = append(result, doc)
		}
	}

	lenght := len(result)
	if lenght == 0 {
		return nil
	}
	cap := min(lenght, 50)

	predicted, err := handleBinaryScore(words, result)
	if err != nil {
		log.Println(err)
		return nil
	}

	for _, rel := range predicted {
		rank[rel.Doc.Id].relation = rel.Score
	}

	sort.Slice(result, func(i, j int) bool {
		return rank[result[i].Id].includesWords > rank[result[j].Id].includesWords || rank[result[i].Id].relation > rank[result[j].Id].relation || 
		rank[result[i].Id].relation == rank[result[j].Id].relation && rank[result[i].Id].includesWords == rank[result[j].Id].includesWords && 
		(rank[result[i].Id].bm25 > rank[result[j].Id].bm25 || rank[result[i].Id].tf_idf > rank[result[j].Id].tf_idf)
	})

	return result[:cap]
}

func (idx *SearchIndex) fetchDocuments(words []string, rank map[uuid.UUID]*requestRanking, index map[string]map[uuid.UUID]int) {
	for _, word := range words {
		if _, ex := index[word]; !ex {
			continue
		}
		for id := range index[word] {
			rank[id].includesWords++
		}
	}
}

func culcBM25(idf float64, tf float64, doc *model.Document, avgLen float64) float64 {
	k1 := 1.2
	b := 0.75
	return idf * (tf * (k1 + 1)) / (tf + k1 * (1 - b + b * doc.GetFullSize() / avgLen))
}

func (idx *SearchIndex) Write(data string) {
	idx.logger.Write(data)
}

func (idx *SearchIndex) IncUrlsCounter() {
	atomic.AddInt32(&idx.UrlsCrawled, 1)
}

func (idx *SearchIndex) AddDocument(doc *model.Document) {
    idx.mu.Lock()
    defer idx.mu.Unlock()
	
	idx.indexRepos.SaveDocument(doc)
	idx.indexRepos.IndexDocument(doc.Id.String(), doc.Words)
}

func (idx *SearchIndex) TokenizeAndStem(text string) []string {
    text = strings.ToLower(text)
    
    var tokens []string
    var currentToken strings.Builder
    
    for _, r := range text {
        if unicode.IsLetter(r) || unicode.IsNumber(r) {
            currentToken.WriteRune(r)
        } else if currentToken.Len() > 0 {
            token := currentToken.String()
            if !idx.stopWords.IsStopWord(token) {
                stemmed := idx.stemmer.Stem(token)
                tokens = append(tokens, stemmed)
            }
            currentToken.Reset()
        }
    }
    
    if currentToken.Len() > 0 {
        token := currentToken.String()
        if !idx.stopWords.IsStopWord(token) {
            stemmed := idx.stemmer.Stem(token)
            tokens = append(tokens, stemmed)
        }
    }
    
    return tokens
}

func (idx *SearchIndex) HandleDocumentWords(doc *model.Document, text string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	
	doc.Words = append(doc.Words, idx.TokenizeAndStem(text)...)
	doc.ArchiveDocument()
}

func (idx *SearchIndex) GetContext() context.Context {
	return idx.quitCTX
}