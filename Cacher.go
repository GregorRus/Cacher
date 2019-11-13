package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
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

func (ri ResponseInfo) Send(resw http.ResponseWriter, req *http.Request) (err error) {
	CopyHeader(ri.Response.Header, resw.Header())
	ri.AddToResponseMetaHeaders(resw)
	_, err = resw.Write(ri.Body)
	return
}

func (ri ResponseInfo) Update(response *http.Response) {
	ri.Response = response
	ri.LastResponseTime = time.Now()
	ri.ExpiresTime = ri.LastResponseTime.Add(ri.LifeTime)
	atomic.StoreUint64(&ri.RequestCount, 0)
	atomic.AddUint64(&ri.UpdateCount, 1)
}

type CacheServer struct {
	config *CacheServerConfig

	upstream        *http.Client
	upstreamAddress string

	cache             map[string]ResponseInfo
	processingWaiters sync.Map

	toSetCache    chan ResponseInfo
	toRemoveCache chan string

	accessLog         *log.Logger
	errorLog          *log.Logger
	cacheLog          *log.Logger
	upstreamAccessLog *log.Logger
}

func NewCacheServer(config *CacheServerConfig) (cs *CacheServer) {
	cs = new(CacheServer)

	cs.config = config

	cs.upstream = new(http.Client)
	cs.upstreamAddress = config.UpstreamAddress
	cs.cache = make(map[string]ResponseInfo)

	cs.toSetCache = make(chan ResponseInfo)
	cs.toRemoveCache = make(chan string)

	logFlags := log.Lshortfile | log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC
	logFileFlags := os.O_WRONLY | os.O_APPEND | os.O_CREATE
	file, err := os.OpenFile(config.AccessLogPath, logFileFlags, 0644)
	if err != nil {
		log.Fatal(err)
	}
	cs.accessLog = log.New(file, "accessLog: ", logFlags)

	file, err = os.OpenFile(config.ErrorLogPath, logFileFlags, 0644)
	if err != nil {
		log.Fatal(err)
	}
	cs.errorLog = log.New(file, "errorLog: ", logFlags)

	file, err = os.OpenFile(config.CacheLogPath, logFileFlags, 0644)
	if err != nil {
		log.Fatal(err)
	}
	cs.cacheLog = log.New(file, "cacheLog: ", logFlags)

	file, err = os.OpenFile(config.UpstreamAccessLogPath, logFileFlags, 0644)
	if err != nil {
		log.Fatal(err)
	}
	cs.upstreamAccessLog = log.New(file, "upstreamAccessLog: ", logFlags)

	return
}

func (cs *CacheServer) ObtainResponse(originalReq *http.Request) (response *http.Response, err error) {
	proxyURLInstance := *originalReq.URL
	proxyURL := &proxyURLInstance
	proxyURL.Host = cs.upstreamAddress
	proxyURL.Scheme = "http"

	proxyReq, err := http.NewRequest(originalReq.Method, proxyURL.String(), originalReq.Body)
	if err != nil {
		cs.errorLog.Printf("Error occured during creating request to upstream \"%s\" (original request to \"%s\"): %v\n",
			proxyURL, originalReq.URL, err,
		)
		return
	}

	proxyReq.Header.Set("Host", originalReq.Host)
	proxyReq.Header.Set("X-Forwarded-For", originalReq.RemoteAddr)

	CopyHeader(originalReq.Header, proxyReq.Header)

	cs.upstreamAccessLog.Printf("Sending request \"%s\" to upstream \"%s\"\n", originalReq.URL, proxyURL)
	response, err = cs.upstream.Do(proxyReq)
	cs.upstreamAccessLog.Printf("Got response of request \"%s\" to upstream \"%s\": %s %d\n",
		originalReq.URL, proxyURL, response.Status, response.ContentLength,
	)
	if err != nil {
		cs.errorLog.Printf("Error occured during proxy \"%s\" to upstream \"%s\": %v\n",
			originalReq.URL, proxyURL, err,
		)
	}
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

func (cs *CacheServer) TrySendFromCache(resw http.ResponseWriter, req *http.Request) (ok bool, err error) {
	path := req.URL.Path
	if resinfoCache, ok := cs.cache[path]; ok {
		atomic.AddUint64(&resinfoCache.RequestCount, 1)
		err = resinfoCache.Send(resw, req)
		if err != nil {
			ok = false
			cs.errorLog.Printf("Sending error occurred during processing \"%s\": %v\n", req.URL, err)
		}
	}
	return
}

func (cs *CacheServer) ServeHTTP(resw http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	path := req.URL.Path

	cs.accessLog.Printf("Processing request \"%s\"\n", req.URL)

	if ok, err := cs.TrySendFromCache(resw, req); !ok {
		if err != nil {
			resw.WriteHeader(http.StatusInternalServerError)
			return
		}

		waitGroupInterface, ok := cs.processingWaiters.LoadOrStore(path, new(sync.WaitGroup))
		waitGroup := waitGroupInterface.(*sync.WaitGroup)
		if !ok {
			waitGroup.Add(1)
			resinfo, err := cs.ObtainFirstResponse(req)
			if err != nil {
				resw.WriteHeader(http.StatusBadGateway)
				cs.errorLog.Printf("Gateway error occurred during processing \"%s\": %v\n", req.URL, err)
				return
			}
			cs.toSetCache <- resinfo
			resinfo.Send(resw, req)
			go cs.Updating(resinfo, resw)
		} else {
			waitGroup.Wait()
			if ok, _ = cs.TrySendFromCache(resw, req); !ok {
				resw.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	}
}

func (cs *CacheServer) CacheUpdating() {
	log.Print("Starting cache updating goroutine\n")

	for true {
		select {
		case resinfo := <-cs.toSetCache:
			cs.cacheLog.Printf("Starting \"%s\" cache entry setting\n", resinfo.Path)
			cs.cache[resinfo.Path] = resinfo
			processingWaiterInterface, ok := cs.processingWaiters.Load(resinfo.Path)
			if ok {
				processingWaiter := processingWaiterInterface.(*sync.WaitGroup)
				processingWaiter.Done()
				cs.processingWaiters.Delete(resinfo.Path)
			}
			cs.cacheLog.Printf("Set \"%s\" cache entry\n", resinfo.Path)
		case path := <-cs.toRemoveCache:
			cs.cacheLog.Printf("Starting \"%s\" cache entry deletion\n", path)
			delete(cs.cache, path)
			cs.cacheLog.Printf("Deleted \"%s\" cache entry\n", path)
		}
	}
}

func (cs *CacheServer) ListenAndServe() {
	log.Print("Starting cache server listening and serving\n")

	http.ListenAndServe(":"+strconv.Itoa(cs.config.ListeningPort), cs)
}

type CacheServerConfig struct {
	GoMaxProcs int

	ListeningPort   int
	UpstreamAddress string

	AccessLogPath         string
	ErrorLogPath          string
	CacheLogPath          string
	UpstreamAccessLogPath string
}

func NewCacheServerConfig() (conf CacheServerConfig) {
	goMaxProcs := flag.Int("proc", 1, "Max go procs count")
	listeningPort := flag.Int("port", 0, "Listening port")
	upstreamAddress := flag.String("upstream", "", "Upstream address")
	accessLogPath := flag.String("access-log", "access.log", "Access log file path")
	errorLogPath := flag.String("error-log", "error.log", "Error log file path")
	cacheLogPath := flag.String("cache-log", "cache.log", "Cache changes log file path")
	upstreamAccessLogPath := flag.String("upstream-log", "upstream.log", "Upstream access log file path")
	flag.Parse()

	if *goMaxProcs == 0 {
		*goMaxProcs = runtime.NumCPU()
	}
	conf.GoMaxProcs = *goMaxProcs
	runtime.GOMAXPROCS(*goMaxProcs)
	log.Printf("Go max proc is %d\n", *goMaxProcs)

	if *listeningPort == 0 {
		log.Fatal("Missing required flag -port")
	}
	conf.ListeningPort = *listeningPort
	log.Printf("Server listening port is %d\n", *listeningPort)

	if *upstreamAddress == "" {
		log.Fatal("Missing required flag -upstream")
	}
	conf.UpstreamAddress = *upstreamAddress
	log.Printf("Upstream server is \"%s\"\n", *upstreamAddress)

	conf.AccessLogPath = *accessLogPath
	conf.ErrorLogPath = *errorLogPath
	conf.CacheLogPath = *cacheLogPath
	conf.UpstreamAccessLogPath = *upstreamAccessLogPath

	return
}

func main() {
	log.Print("Initializing\n")

	config := NewCacheServerConfig()
	cacher := NewCacheServer(&config)

	go cacher.CacheUpdating()

	cacher.ListenAndServe()
}
