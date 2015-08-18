package hkpfront

import (
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/andres-erbsen/clock"
	"github.com/yahoo/coname"
	"github.com/yahoo/coname/proto"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/net/context"
)

var responseHeaderTemplate = template.Must(template.New("hkpfrontGetResponseHeader").Parse(
	`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd" >
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
<meta http-equiv="Content-Type" content="text/html;charset=utf-8" />
<title>Public Key Server -- Get "{{.}}"</title>
</head>
<body><h1>Public Key Server -- Get "{{.}}"</h1>
<pre>
`))
var responseFooter = "\n</pre></body></html>"

// HKPFront implements a unverified GnuPG-compatible HKP frontend for the
// verified keyserver.
type HKPFront struct {
	Lookup func(context.Context, *proto.LookupRequest) (*proto.LookupProof, error)
	Clk    clock.Clock

	InsecureSkipVerify bool
	// Config must be set and valid if InsecureSkipVerify is not set
	Config *proto.Config

	ln net.Listener
	sr http.Server

	connStateMu sync.Mutex
	connState   map[net.Conn]http.ConnState

	stopOnce sync.Once
	stop     chan struct{}
	waitStop sync.WaitGroup // server + all open connections
}

func (h *HKPFront) Start(ln net.Listener) {
	h.stop = make(chan struct{})
	h.connState = make(map[net.Conn]http.ConnState)
	h.sr = http.Server{
		Addr:           ln.Addr().String(),
		Handler:        h,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 4096,
		ConnState:      h.updateConnState,
	}
	h.ln = ln
	h.waitStop.Add(1)
	go h.run()
}

func (h *HKPFront) run() {
	defer h.waitStop.Done()
	h.sr.Serve(h.ln)
}

func (h *HKPFront) Stop() {
	h.stopOnce.Do(func() {
		close(h.stop)
		h.sr.SetKeepAlivesEnabled(false)
		h.ln.Close()

		h.connStateMu.Lock()
		for c, s := range h.connState {
			if s == http.StateIdle {
				c.Close()
			}
		}
		h.connStateMu.Unlock()

		h.waitStop.Wait()
	})
}

func (h *HKPFront) updateConnState(c net.Conn, s http.ConnState) {
	h.connStateMu.Lock()
	defer h.connStateMu.Unlock()
	h.connState[c] = s
	switch s {
	case http.StateNew:
		h.waitStop.Add(1)
	case http.StateIdle:
		select {
		case <-h.stop:
			c.Close()
		default:
		}
	case http.StateClosed, http.StateHijacked:
		h.waitStop.Done()
		delete(h.connState, c)
	}
}

func (h *HKPFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Method != "GET" || r.URL.Path != "/pks/lookup" || len(q["op"]) != 1 || q["op"][0] != "get" || len(q["search"]) != 1 {
		http.Error(w, `this server only supports queries of the form "/pks/lookup?op=get&search=<EMAIL>"`, 501)
		return
	}
	user := q["search"][0]
	ctx := context.Background()

	var requiredSignatures *proto.QuorumExpr
	if !h.InsecureSkipVerify {
		realm, err := coname.GetRealmByUser(h.Config, user)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		requiredSignatures = realm.VerificationPolicy.Quorum
	}

	pf, err := h.Lookup(ctx, &proto.LookupRequest{UserId: user, QuorumRequirement: requiredSignatures})
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}

	if !h.InsecureSkipVerify {
		coname.VerifyLookup(h.Config, user, pf, h.Clk.Now())
	}

	if pf.Profile.Keys == nil {
		http.Error(w, `No results found: No keys found: unknown email`, 404)
		return
	}

	pgpKey, present := pf.Profile.Keys["pgp"]
	if !present {
		http.Error(w, `No results found: No keys found: the email is known to the keyserver, but the profile does not include an OpenPGP key`, 404)
		return
	}
	if err := responseHeaderTemplate.Execute(w, user); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	aw, err := armor.Encode(w, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, err = aw.Write(pgpKey)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := aw.Close(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, err = w.Write([]byte(responseFooter))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
}
