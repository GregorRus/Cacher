package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func CopyHeader(source http.Header, destination http.Header) {
	for header, values := range source {
		for _, value := range values {
			destination.Add(header, value)
		}
	}
}

type ResponseInfo struct {
	Path              string
	Response          *http.Response
	OriginalRequest   *http.Request
	Body              []byte
	FirstResponseTime time.Time
	LastResponseTime  time.Time
	LifeTime          time.Duration
	ExpiresTime       time.Time
	UpdateTicker      *time.Ticker
	RequestCount      uint64
	UpdateCount       uint64
	Hash              [32]byte
}

func NewResponseInfo(originalRequest *http.Request, response *http.Response) (ri ResponseInfo, err error) {
	now := time.Now()
	lifetime := time.Second * 30
	body := make([]byte, response.ContentLength)
	_, err = io.ReadFull(response.Body, body)
	if err != nil {
		return
	}
	ri = ResponseInfo{
		Path:              originalRequest.URL.Path,
		Response:          response,
		OriginalRequest:   originalRequest,
		Body:              body,
		FirstResponseTime: now,
		LastResponseTime:  now,
		LifeTime:          lifetime,
		ExpiresTime:       now.Add(lifetime),
		UpdateTicker:      time.NewTicker(lifetime),
		RequestCount:      1,
		UpdateCount:       1,
		Hash:              sha256.Sum256(body),
	}
	return
}

func (ri ResponseInfo) AddToResponseMetaHeaders(resw http.ResponseWriter) {
	resw.Header().Set("Date", ri.LastResponseTime.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	resw.Header().Set("Last-Modified", ri.LastResponseTime.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	resw.Header().Set("Expires", ri.ExpiresTime.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	resw.Header().Set("Age", strconv.FormatUint(uint64(time.Now().Sub(ri.LastResponseTime).Seconds()), 10))
	resw.Header().Set("ETag", "\""+hex.EncodeToString(ri.Hash[:])+"\"")

	resw.Header().Add("Cache-Control", "public")
	resw.Header().Add("Cache-Control", "only-if-cached")
	resw.Header().Add("Cache-Control", "max-age="+strconv.FormatUint(uint64(time.Now().Sub(ri.LastResponseTime).Seconds()), 10))
	resw.Header().Add("Cache-Control", "max-stale="+strconv.FormatUint(uint64(ri.LifeTime/time.Second), 10))
}

func (ri ResponseInfo) Send(resw http.ResponseWriter, req *http.Request) {
	CopyHeader(ri.Response.Header, resw.Header())
	ri.AddToResponseMetaHeaders(resw)
	resw.Write(ri.Body)
}

func (ri ResponseInfo) Update(response *http.Response) {
	ri.Response = response
	ri.LastResponseTime = time.Now()
	ri.ExpiresTime = ri.LastResponseTime.Add(ri.LifeTime)
	atomic.StoreUint64(&ri.RequestCount, 0)
	atomic.AddUint64(&ri.UpdateCount, 1)
}

type CacheServer struct {
	upstream        *http.Client
	upstreamAddress string

	cache             map[string]ResponseInfo
	processingWaiters sync.Map

	toSetCache    chan ResponseInfo
	toRemoveCache chan string
}

func NewCacheServer(upstreamAddr string) (cs *CacheServer) {
	cs = new(CacheServer)

	cs.upstream = new(http.Client)
	cs.upstreamAddress = upstreamAddr
	cs.cache = make(map[string]ResponseInfo)

	cs.toSetCache = make(chan ResponseInfo)
	cs.toRemoveCache = make(chan string)

	return
}

func (cs *CacheServer) ObtainResponse(originalReq *http.Request) (response *http.Response, err error) {
	proxyURLInstance := *originalReq.URL
	proxyURL := &proxyURLInstance
	proxyURL.Host = cs.upstreamAddress
	proxyURL.Scheme = "http"

	proxyReq, err := http.NewRequest(originalReq.Method, proxyURL.String(), originalReq.Body)
	if err != nil {
		return
	}

	proxyReq.Header.Set("Host", originalReq.Host)
	proxyReq.Header.Set("X-Forwarded-For", originalReq.RemoteAddr)

	CopyHeader(originalReq.Header, proxyReq.Header)

	response, err = cs.upstream.Do(proxyReq)
	return
}

func (cs *CacheServer) ObtainFirstResponse(originalReq *http.Request) (resinfo ResponseInfo, err error) {
	response, err := cs.ObtainResponse(originalReq)
	defer response.Body.Close()
	if err != nil {
		return
	}
	resinfo, err = NewResponseInfo(originalReq, response)
	return
}

func (cs *CacheServer) ObtainUpdate(ri ResponseInfo, resw http.ResponseWriter) bool {
	if ri.RequestCount > 1 {
		response, err := cs.ObtainResponse(ri.OriginalRequest)
		if err != nil {
			cs.toRemoveCache <- ri.Path
			return false
		}
		ri.Update(response)
	} else {
		cs.toRemoveCache <- ri.Path
		return false
	}
	return true
}

func (cs *CacheServer) Updating(ri ResponseInfo, resw http.ResponseWriter) {
	for range ri.UpdateTicker.C {
		if !cs.ObtainUpdate(ri, resw) {
			break
		}
	}
}

func (cs *CacheServer) TrySendFromCache(resw http.ResponseWriter, req *http.Request) (ok bool) {
	path := req.URL.Path
	if resinfoCache, ok := cs.cache[path]; ok {
		atomic.AddUint64(&resinfoCache.RequestCount, 1)
		resinfoCache.Send(resw, req)
	}
	return
}

func (cs *CacheServer) ServeHTTP(resw http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	path := req.URL.Path

	if !cs.TrySendFromCache(resw, req) {
		waitGroupInterface, ok := cs.processingWaiters.LoadOrStore(path, new(sync.WaitGroup))
		waitGroup := waitGroupInterface.(*sync.WaitGroup)
		if !ok {
			waitGroup.Add(1)
			resinfo, err := cs.ObtainFirstResponse(req)
			if err != nil {
				resw.WriteHeader(http.StatusBadGateway)
				return
			}
			cs.toSetCache <- resinfo
			resinfo.Send(resw, req)
			go cs.Updating(resinfo, resw)
		} else {
			waitGroup.Wait()
			if !cs.TrySendFromCache(resw, req) {
				resw.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	}
}

func (cs *CacheServer) CacheUpdating() {
	for true {
		select {
		case resinfo := <-cs.toSetCache:
			cs.cache[resinfo.Path] = resinfo
			processingWaiterInterface, ok := cs.processingWaiters.Load(resinfo.Path)
			if ok {
				processingWaiter := processingWaiterInterface.(*sync.WaitGroup)
				processingWaiter.Done()
				cs.processingWaiters.Delete(resinfo.Path)
			}
		case path := <-cs.toRemoveCache:
			delete(cs.cache, path)
		}
	}
}

func main() {
	goMaxProcs := flag.Int("proc", 1, "Max go procs count")
	listeningPort := flag.Int("port", 0, "Listening port")
	upstreamAddress := flag.String("upstream", "", "Upstream address")
	flag.Parse()

	if *goMaxProcs == 0 {
		*goMaxProcs = runtime.NumCPU()
	}
	runtime.GOMAXPROCS(*goMaxProcs)

	cacher := NewCacheServer(*upstreamAddress)
	go cacher.CacheUpdating()
	http.ListenAndServe(":"+strconv.Itoa(*listeningPort), cacher)
}
