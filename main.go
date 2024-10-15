package main

import (
	"encoding/json"
	"github.com/fiorix/go-smpp/smpp"
	"github.com/fiorix/go-smpp/smpp/pdu"
	"github.com/fiorix/go-smpp/smpp/pdu/pdufield"
	"github.com/fiorix/go-smpp/smpp/pdu/pdutext"
	"github.com/fiorix/go-smpp/smpp/pdu/pdutlv"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"golang.org/x/time/rate"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

type Config struct {
	Name     string
	Botid    string
	Botkey   string
	Chatid   string
	Address  string
	Smpp     string
	Username string
	Password string
	Debug    int
}

var config = new(Config)

func sendMessage(m string) {
	apiURL := "https://api.telegram.org/" + config.Botid + ":" + config.Botkey + "/sendMessage?chat_id=" + config.Chatid + "&disable_web_page_preview=true&parse_mode=HTML&text="
	if config.Debug < 2 {
		log.Printf("Request: %s%s", apiURL, m)
	}
	apiURL = apiURL + url.QueryEscape(m)
	if config.Debug < 3 {
		log.Printf("Telegram API request: %s", apiURL)
	}
	client := &http.Client{Timeout: time.Second * 10}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("Can't create message for Telegram. Error: %s", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Can't send message to Telegram. Error: %s", err)
	}
	defer resp.Body.Close()

	if err != nil {
		log.Printf("Can't send message to Telegram. Error: %s", err)
	} else {
		bodyText, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Can't get answer from Telegram. Error: %s", err)
		} else {
			s := string(bodyText)
			if resp.StatusCode != 200 {
				log.Printf("Unexpected answer from Telegram! I get: %s", s)
			}
		}
	}
}

func readConfig() {

	file, _ := os.ReadFile("/etc/telegram-smpp/conf.json")
	err := json.Unmarshal(file, &config)
	if err != nil {
		log.Fatalf("Error %s when config read... Stop.", err)
	}
	log.Printf("Program name: %s, bot ID: %s, Chat ID: %s, Listen address: %s, SMPP address: %s", config.Name, config.Botid, config.Chatid, config.Address, config.Smpp)
}

func main() {

	readConfig()

	// Make an tranformer that converts MS-Win default to UTF8:
	win16be := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
	// Make a transformer that is like win16be, but abides by BOM:
	utf16bom := unicode.BOMOverride(win16be.NewDecoder())

	f := func(p pdu.Body) {
		if config.Debug < 2 {
			log.Printf("Message: %q", p)
		}
		switch p.Header().ID {
		case pdu.DeliverSMID:
			f := p.Fields()
			tlv := p.TLVFields()
			coding := f[pdufield.DataCoding]
			src := f[pdufield.SourceAddr]
			dst := f[pdufield.DestinationAddr]
			txt := f[pdufield.ShortMessage]
			longtext := tlv[pdutlv.TagMessagePayload]
			var text string
			var err error
			if config.Debug < 2 {
				log.Printf("ShortMessage: %q, TagMessagePayload: %q, Coding: %q", txt, longtext, coding)
			}
			if txt.String() == "" {
				txt = longtext
			}
			if coding.String() == "8" {
				text, _, err = transform.String(utf16bom, txt.String())
				if err != nil {
					log.Printf("Can't decode UTF16 message %q", txt)
				}
			} else {
				text = txt.String()
			}
			if config.Debug < 2 {
				log.Printf("Text: %q", text)
			}
			sendMessage("SMS from " + src.String() + " to " + dst.String() + " :\n" + text)
		}
	}
	lm := rate.NewLimiter(rate.Limit(10), 1) // Max rate of 10/s.
	tx := &smpp.Transceiver{
		Addr:        config.Smpp,
		User:        config.Username,
		Passwd:      config.Password,
		Handler:     f,  // Handle incoming SM or delivery receipts.
		RateLimiter: lm, // Optional rate limiter.
	}
	// Create persistent connection.
	conn := tx.Bind()
	go func() {
		for c := range conn {
			log.Printf("SMPP connection status: %q", c.Status())
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sm, err := tx.Submit(&smpp.ShortMessage{
			Src:      r.FormValue("src"),
			Dst:      r.FormValue("dst"),
			Text:     pdutext.Raw(r.FormValue("text")),
			Register: pdufield.FinalDeliveryReceipt,
		})
		if err == smpp.ErrNotConnected {
			http.Error(w, "Oops.", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		io.WriteString(w, sm.RespID())
	})
	log.Fatal(http.ListenAndServe(config.Address, nil))
}
