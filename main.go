package main

import (
	"encoding/json"
	"fmt"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
)

// MadnsConfig - Structure for JSON config files
type MadnsConfig struct {
	SMTPUser     string
	SMTPPassword string

	// make this "hostname:port", "smtp.gmail.com:587" for gmail+TLS
	SMTPServer string

	// number of seconds to aggregate responses before sending an email
	SMTPDelay int

	Port     int
	Handlers map[string]MadnsSubConfig
}

// MadnsSubConfig - Structure for Subdomain portion of JSON config files
type MadnsSubConfig struct {
	Redirect    string
	NotifyEmail string
	Respond     string
	NotifySlack string
	Rebind	    *MadnsRebindConfig
}

// MadnsRebindConfig - Rebind requires more data and I'd like to add strategies one day
type MadnsRebindConfig struct {
	Addrs []string
}

// Yes, big ugly global variable, but :shrug:
var RebindMap sync.Map = sync.Map{}

func main() {

	var config MadnsConfig
	usage := flag.Bool("h", false, "Show usage")
	configFile := flag.String("c", "madns-config.json", "madns JSON Config File")
	flag.Parse()

	b, err := os.ReadFile(*configFile)
	if err != nil || *usage {
		if err != nil {
			slog.Error(err.Error())
		}
		flag.Usage()
		return
	}
	if err = json.Unmarshal(b, &config); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	listenString := ":" + strconv.Itoa(config.Port)

	dns.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		handleDNS(w, req, config)
	}) // pattern-matching of HandleFunc sucks, have to do our own

	go serve("tcp", listenString)
	go serve("udp", listenString)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig)
	signal.Ignore(syscall.SIGURG)
	for {
		select {
		case s := <-sig:
			slog.Error(fmt.Sprintf("signal %s received\n", s))
			os.Exit(1)
		}
	}
}

func serve(net, addr string) {
	server := &dns.Server{Addr: addr, Net: net, TsigSecret: nil}
	err := server.ListenAndServe()
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to setup the %s server: %v\n", net, err))
		os.Exit(1)
	}
}

func handleDNS(w dns.ResponseWriter, req *dns.Msg, config MadnsConfig) {

	// DETERMINE WHICH CONFIG APPLIES
	var c MadnsSubConfig
	var ck string
	processThis := false
	for k, v := range config.Handlers {
		if k == "." { // check default case last
			continue
		}
		reqFqdn := strings.ToLower(req.Question[0].Name)
		handlerFqdn := strings.ToLower(dns.Fqdn(k))

		if reqFqdn == handlerFqdn || strings.HasSuffix(reqFqdn, "."+handlerFqdn) {
			ck = k
			c = v
			processThis = true
			break
		}
	}
	if !processThis {
		cnf, ok := config.Handlers["."] // is there a default handler?
		if ok {
			ck = "."
			c = cnf
			processThis = true
		}
	}
	if !processThis {
		slog.Warn("no handler for domain: " + req.Question[0].Name)
		m := new(dns.Msg)
		m.SetReply(req)
		m.SetRcode(req, dns.RcodeServerFailure)
		w.WriteMsg(m)
		return // no subsequent handling
	}

	// REDIRECT, if desired (mutually exclusive with RESPOND)
	if len(c.Redirect) > 0 {
		handleRedirect(w,req,c.Redirect)
	// RESPOND, if desired (mutually exclusive with REDIRECT)
	} else if len(c.Respond) > 0 {
		handleRespond(w,req,c.Respond)
	} else if c.Rebind != nil && len(c.Rebind.Addrs) > 0 {
		rc := c.Rebind
		// Do round robin on the list of addrs (but concurrently-safe)
		ctrAny, _ := RebindMap.LoadOrStore(ck, &atomic.Uint64{})
		ctr, _ := ctrAny.(*atomic.Uint64)
		respond := rc.Addrs[(ctr.Add(1) - 1) % uint64(len(rc.Addrs))] // We want the pre-increment value, hence -1
		handleRespond(w,req,respond)
	}

	body := "source: " + w.RemoteAddr().String() + "\n" +
		"proto: " + w.RemoteAddr().Network() + "\n" +
		"request:\n" + req.String() + "\n\n"

	// EMAIL NOTIFICATION, if directed
	if len(c.NotifyEmail) > 0 {
		debouncedSendEmail(c.NotifyEmail, body, config)
	}

	// Slack Notification, if directed
	if len(c.NotifySlack) > 0 {
		sendSlackMessage(c.NotifySlack, body)
	}
}

func handleRedirect(w dns.ResponseWriter, req *dns.Msg, redirect string) {
	dnsClient := &dns.Client{Net: "udp", ReadTimeout: 4 * time.Second, WriteTimeout: 4 * time.Second, SingleInflight: true}
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		dnsClient.Net = "tcp"
	}

	slog.Info("redirecting using protocol: " + dnsClient.Net)

	retries := 1
	retry:
	r, _, err := dnsClient.Exchange(req, redirect)
	if err == nil {
		r.Compress = true
		w.WriteMsg(r)
	} else {
		if retries > 0 {
			retries--
			slog.Debug("retrying...")
			goto retry
		} else {
			slog.Warn(fmt.Sprintf("failure to forward request %q\n", err))
			m := new(dns.Msg)
			m.SetReply(req)
			m.SetRcode(req, dns.RcodeServerFailure)
			w.WriteMsg(m)
		}
	}
}

func handleRespond(w dns.ResponseWriter, req *dns.Msg, respond string) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.SetRcode(req, dns.RcodeSuccess)

	m.Answer = make([]dns.RR, len(req.Question))
	for i := range req.Question {
		slog.Info("Responding to " + req.Question[i].Name + " with " + respond)

		ip := net.ParseIP(respond)
		if ip == nil {
			// This is not a valid IP address, so assume it's a CNAME
			rr := new(dns.CNAME)
			rr.Hdr = dns.RR_Header{Name: req.Question[i].Name,
			Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 0}
			rr.Target = strings.TrimSuffix(respond, ".") + "."
			m.Answer[i] = rr
		} else if ip.To4() == nil {
			// This is an IPv6 address, so do a AAAA record
			rr := new(dns.AAAA)
			rr.Hdr = dns.RR_Header{Name: req.Question[i].Name,
			Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 0}
			rr.AAAA = ip
			m.Answer[i] = rr
		} else {
			// This is an IPv4 address, so do an A record
			rr := new(dns.A)
			rr.Hdr = dns.RR_Header{Name: req.Question[i].Name,
			Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}
			rr.A = ip
			m.Answer[i] = rr
		}
	}
	w.WriteMsg(m)
}
