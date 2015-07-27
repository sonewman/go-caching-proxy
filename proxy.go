package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/sonewman/rox"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

func main() {
	host := flag.String("host", "", "define host to be forwarded")
	address := flag.String("address", ":8080", "define address proxy will run on")
	//cookieDomain := flag.String("domain", "", "define cookie domain")
	//followProtocol := flag.Bool("r", false, "should retain scheme on redirect")
	cache := flag.Bool("c", false, "caches responses")
	log := flag.Bool("l", false, "log incoming request")
	ttl := flag.Int("ttl", -1, "cache TTL")

	flag.Parse()

	//	if *cookieDomain == "" {
	//		cookieDomain = host
	//	}

	var target *url.URL

	if len(flag.Args()) > 0 {
		fwd := strings.Join(flag.Args()[0:1], "")

		if fwd != "" {
			u, err := url.Parse(fwd)
			if err != nil {
				panic(err)
			}

			target = u
		}
	}

	addresses := strings.Split(*address, ",")
	al := len(addresses)
	i := 0

	for _, add := range addresses {
		opts := &options{
			Target:  target,
			Address: add,
			Host:    host,
			Cache:   cache,
			TTL:     ttl,
			Log:     log,
		}

		i += 1
		if i == al {
			createProxy(opts)
		} else {
			go createProxy(opts)
		}
	}
}

type options struct {
	Target  *url.URL
	Address string
	Host    *string
	Cache   *bool
	TTL     *int
	Log     *bool
}

func ensureHost(out *http.Request, o *options) {
	if *o.Host != "" {
		out.Host = *o.Host
	}
}

func maybeLog(o *options, out *http.Request) {
	if *o.Log == true {
		log.Println(fmt.Sprintf("%s %s", out.Method, out.URL))
	}
}

func cacheHandle(o *options) func(*rox.Rox, http.ResponseWriter, *http.Request, *http.Request) {
	cache := &Cache{
		cache: make(map[string]*CachedResponse),
	}

	return func(p *rox.Rox, rw http.ResponseWriter, in *http.Request, out *http.Request) {
		ensureHost(out, o)
		rox.PrepareRequest(out)

		cr := cache.Get(out)
		if cr != nil {
			io.Copy(rw, cr)
			maybeLog(o, out)
			return
		}

		cr = cache.Create(out)

		res, err := rox.DoRequest(p, out)
		maybeLog(o, out)

		if res != nil {
			defer res.Body.Close()
		}

		if err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		cr.Set(res, *o.TTL)
		// pull out
		select {
		case <-cr.UpdateChan:
			cr.UpdateChan = nil
		default:
			cr.UpdateChan = nil
		}
		io.Copy(rw, cr)
	}
}

func regularRequest(o *options) func(*rox.Rox, http.ResponseWriter, *http.Request, *http.Request) {
	return func(p *rox.Rox, rw http.ResponseWriter, in *http.Request, out *http.Request) {
		ensureHost(out, o)
		rox.DefaultMakeRequest(p, rw, in, out)
		maybeLog(o, out)
	}
}

func createMakeRequest(o *options) func(*rox.Rox, http.ResponseWriter, *http.Request, *http.Request) {
	if *o.Cache == true {
		return cacheHandle(o)
	}

	return regularRequest(o)
}

func createProxy(o *options) {
	makeRequest := createMakeRequest(o)

	proxy := &rox.Rox{
		MakeRequest: makeRequest,
		Target:      o.Target,
	}

	log.Println(fmt.Sprintf("starting proxy server at address %s", o.Address))
	log.Fatal(http.ListenAndServe(o.Address, proxy))
}

type CachedResponse struct {
	lk         sync.Mutex
	Header     http.Header
	StatusCode int
	Body       []byte
	UpdateChan chan error
}

func (cr *CachedResponse) Write(p []byte) (int, error) {
	cl := len(cr.Body)
	nl := cl + len(p)
	if nl > cl {
		updated := make([]byte, nl)
		copy(updated, cr.Body)
		n := copy(updated[cl:nl], p)
		cr.Body = updated
		return n, nil
	}

	return 0, nil
}

func (cr *CachedResponse) Read(p []byte) (int, error) {
	if cr.Body != nil {
		return copy(p, cr.Body), nil
	}

	return 0, errors.New("Cached Request Body is nil")
}

func (cr *CachedResponse) WriteTo(w io.Writer) (int64, error) {
	b := make([]byte, len(cr.Body))
	_, er := cr.Read(b)

	if er != nil {
		return 0, er
	}

	if hrw, ok := w.(http.ResponseWriter); ok {
		rox.CopyHeader(hrw.Header(), cr.Header)
		hrw.WriteHeader(cr.StatusCode)
		w = hrw
	}

	nw, err := w.Write(b)

	if err != nil {
		return 0, err
	}

	return int64(nw), nil
}

func (cr *CachedResponse) Close() error {
	return nil
}

func (cr *CachedResponse) Set(res *http.Response, TTL int) {
	header := make(http.Header)
	rox.CopyHeader(header, res.Header)
	cr.Header = header
	cr.StatusCode = res.StatusCode
	io.Copy(cr, res.Body)

	defer func() {
		go cr.completeUpdate()
	}()
}

func (cr *CachedResponse) completeUpdate() {
	if cr.UpdateChan != nil {
		cr.UpdateChan <- nil
	}
}

type Cache struct {
	lk    sync.Mutex
	cache map[string]*CachedResponse
}

func getKey(r *http.Request) string {
	var query string

	if r.URL.RawQuery != "" {
		q := []string{"?", r.URL.RawQuery}
		query = strings.Join(q, "")
	}

	s := []string{r.Method, r.URL.Scheme, r.URL.Host, r.URL.Path, query}
	return strings.Join(s, "")
}

func (c *Cache) Get(req *http.Request) *CachedResponse {
	key := getKey(req)

	var cached *CachedResponse

	if c.cache[key] != nil {
		// check ttl
		cached = c.cache[key]

		// if the update channel is
		// available then we can block
		// until this is ready to consume
		if cached.UpdateChan != nil {
		pendingUpdate:
			for {
				select {
				case err := <-cached.UpdateChan:
					if err != nil {
						panic(err)
					}
				default:
					break pendingUpdate
				}
			}
		}
	}

	return cached
}

func (c *Cache) Create(req *http.Request) *CachedResponse {
	c.lk.Lock()
	key := getKey(req)
	c.cache[key] = &CachedResponse{UpdateChan: make(chan error)}
	defer c.lk.Unlock()
	return c.cache[key]
}
