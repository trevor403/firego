/*
Package firego is a REST client for Firebase (https://firebase.com).
*/
package firego

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_url "net/url"
	"strings"
	"sync"
	"time"
)

// TimeoutDuration is the length of time any request will have to establish
// a connection and receive headers from Firebase before returning
// an ErrTimeout error.
var TimeoutDuration = 30 * time.Second

var defaultRedirectLimit = 30

// ErrTimeout is an error type is that is returned if a request
// exceeds the TimeoutDuration configured.
type ErrTimeout struct {
	error
}

// query parameter constants
const (
	authParam         = "auth"
	shallowParam      = "shallow"
	formatParam       = "format"
	formatVal         = "export"
	orderByParam      = "orderBy"
	limitToFirstParam = "limitToFirst"
	limitToLastParam  = "limitToLast"
	startAtParam      = "startAt"
	endAtParam        = "endAt"
	equalToParam      = "equalTo"
)

const defaultHeartbeat = 2 * time.Minute

type Auth struct {
	mux   sync.RWMutex
	token string
}

func NewAuth(token string) *Auth {
	auth := &Auth{}
	auth.Set(token)
	return auth
}

// Set will set the custom Firebase token used to authenticate to Firebase.
func (a *Auth) Set(token string) {
	a.mux.Lock()
	a.token = token
	a.mux.Unlock()
}

// Get returns the current token being used to authenticate to Firebase.
func (a *Auth) Get() string {
	a.mux.RLock()
	defer a.mux.RUnlock()
	return a.token
}

// Firebase represents a location in the cloud.
type Firebase struct {
	url           string
	client        *http.Client
	clientTimeout time.Duration

	sharedAuth *Auth

	paramsMtx sync.RWMutex
	params    _url.Values

	eventMtx   sync.Mutex
	eventFuncs map[string]chan struct{}

	watchMtx       sync.Mutex
	watching       bool
	watchHeartbeat time.Duration
	stopWatching   chan struct{}
}

// New creates a new Firebase reference,
// if client is nil, http.DefaultClient is used.
func New(url string, client *http.Client) *Firebase {
	fb := &Firebase{
		url:            sanitizeURL(url),
		params:         _url.Values{},
		clientTimeout:  TimeoutDuration,
		stopWatching:   make(chan struct{}),
		watchHeartbeat: defaultHeartbeat,
		eventFuncs:     map[string]chan struct{}{},
	}
	if client == nil {
		var tr *http.Transport
		tr = &http.Transport{
			Dial: func(network, address string) (net.Conn, error) {
				start := time.Now()
				c, err := net.DialTimeout(network, address, fb.clientTimeout)
				tr.ResponseHeaderTimeout = fb.clientTimeout - time.Since(start)
				return c, err
			},
		}

		client = &http.Client{
			Transport:     tr,
			CheckRedirect: redirectPreserveHeaders,
		}
	}

	fb.client = client
	return fb
}

// Auth sets the custom Firebase token used to authenticate to Firebase.
func (fb *Firebase) Auth(token string) {
	fb.paramsMtx.Lock()
	fb.params.Set(authParam, token)
	fb.paramsMtx.Unlock()
}

// Unauth removes the current token being used to authenticate to Firebase.
func (fb *Firebase) Unauth() {
	fb.paramsMtx.Lock()
	fb.params.Del(authParam)
	fb.paramsMtx.Unlock()
}

// SetSharedAuth adds a referance to a shared auth token
func (fb *Firebase) SetSharedAuth(auth *Auth) {
	fb.paramsMtx.Lock()
	fb.sharedAuth = auth
	fb.paramsMtx.Unlock()
}

// Ref returns a copy of an existing Firebase reference with a new path.
func (fb *Firebase) Ref(path string) (*Firebase, error) {
	newFB := fb.copy()
	parsedURL, err := _url.Parse(fb.url)
	if err != nil {
		return newFB, err
	}
	newFB.url = parsedURL.Scheme + "://" + parsedURL.Host + "/" + strings.Trim(path, "/")
	return newFB, nil
}

// SetURL changes the url for a firebase reference.
func (fb *Firebase) SetURL(url string) {
	fb.url = sanitizeURL(url)
}

// URL returns firebase reference URL
func (fb *Firebase) URL() string {
	return fb.url
}

// Push creates a reference to an auto-generated child location.
func (fb *Firebase) Push(v interface{}) (*Firebase, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	_, bytes, err = fb.doRequest("POST", bytes)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(bytes, &m); err != nil {
		return nil, err
	}
	newRef := fb.copy()
	newRef.url = fb.url + "/" + m["name"]
	return newRef, err
}

// Remove the Firebase reference from the cloud.
func (fb *Firebase) Remove() error {
	_, _, err := fb.doRequest("DELETE", nil)
	if err != nil {
		return err
	}
	return nil
}

// Set the value of the Firebase reference.
func (fb *Firebase) Set(v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, _, err = fb.doRequest("PUT", bytes)
	return err
}

// Update the specific child with the given value.
func (fb *Firebase) Update(v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, _, err = fb.doRequest("PATCH", bytes)
	return err
}

// Get gets the value of the Firebase reference.
func (fb *Firebase) Get(v interface{}) error {
	return fb.Value(v)
}

// Value gets the value of the Firebase reference.
func (fb *Firebase) Value(v interface{}) error {
	_, bytes, err := fb.doRequest("GET", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

// String returns the string representation of the
// Firebase reference.
func (fb *Firebase) String() string {
	path := fb.url + "/.json"

	fb.paramsMtx.RLock()
	params := _url.Values{}
	if len(fb.params) > 0 {
		for k, v := range fb.params {
			params[k] = v
		}
	}

	if fb.sharedAuth != nil {
		params.Set(authParam, fb.sharedAuth.Get())
	}

	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	fb.paramsMtx.RUnlock()
	return path
}

// Child creates a new Firebase reference for the requested
// child with the same configuration as the parent.
func (fb *Firebase) Child(child string) *Firebase {
	c := fb.copy()
	c.url = c.url + "/" + child
	return c
}

func (fb *Firebase) copy() *Firebase {
	c := &Firebase{
		url:            fb.url,
		params:         _url.Values{},
		client:         fb.client,
		clientTimeout:  fb.clientTimeout,
		sharedAuth:     fb.sharedAuth,
		stopWatching:   make(chan struct{}),
		watchHeartbeat: defaultHeartbeat,
		eventFuncs:     map[string]chan struct{}{},
	}

	// making sure to manually copy the map items into a new
	// map to avoid modifying the map reference.
	fb.paramsMtx.RLock()
	for k, v := range fb.params {
		c.params[k] = v
	}
	fb.paramsMtx.RUnlock()
	return c
}

func sanitizeURL(url string) string {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		url = "https://" + url
	}

	if strings.HasSuffix(url, "/") {
		url = url[:len(url)-1]
	}

	return url
}

// Preserve headers on redirect.
//
// Reference https://github.com/golang/go/issues/4800
func redirectPreserveHeaders(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// No redirects
		return nil
	}

	if len(via) > defaultRedirectLimit {
		return fmt.Errorf("%d consecutive requests(redirects)", len(via))
	}

	// mutate the subsequent redirect requests with the first Header
	for key, val := range via[0].Header {
		req.Header[key] = val
	}
	return nil
}

func withHeader(key, value string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Add(key, value)
	}
}

func (fb *Firebase) doRequest(method string, body []byte, options ...func(*http.Request)) (http.Header, []byte, error) {
	req, err := http.NewRequest(method, fb.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	for _, opt := range options {
		opt(req)
	}

	resp, err := fb.client.Do(req)
	switch err := err.(type) {
	default:
		return nil, nil, err
	case nil:
		// carry on

	case *_url.Error:
		// `http.Client.Do` will return a `url.Error` that wraps a `net.Error`
		// when exceeding it's `Transport`'s `ResponseHeadersTimeout`
		e1, ok := err.Err.(net.Error)
		if ok && e1.Timeout() {
			return nil, nil, ErrTimeout{err}
		}

		return nil, nil, err

	case net.Error:
		// `http.Client.Do` will return a `net.Error` directly when Dial times
		// out, or when the Client's RoundTripper otherwise returns an err
		if err.Timeout() {
			return nil, nil, ErrTimeout{err}
		}

		return nil, nil, err
	}

	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode/200 != 1 {
		return resp.Header, respBody, errors.New(string(respBody))
	}
	return resp.Header, respBody, nil
}
