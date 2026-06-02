package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/fiorix/go-smpp/smpp"
	"github.com/fiorix/go-smpp/smpp/pdu"
	"github.com/fiorix/go-smpp/smpp/pdu/pdufield"
	"github.com/fiorix/go-smpp/smpp/pdu/pdutext"
	"github.com/fiorix/go-smpp/smpp/pdu/pdutlv"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"golang.org/x/time/rate"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	telegramQueueSize   = 1000
	telegramRetryDelay  = 5 * time.Second
	telegramHTTPTimeout = 30 * time.Second
)

type Config struct {
	Name      string
	Botid     string
	Botkey    string
	Chattype  string
	Chatid    string
	Chattopic string
	Address   string
	Smpp      string
	Username  string
	Password  string
	Debug     int
}

var config = new(Config)
var telegramQueue chan string
var telegramHTTPClient = &http.Client{Timeout: telegramHTTPTimeout}

func createForm(form map[string]string) (string, io.Reader, error) {
	body := new(bytes.Buffer)
	mp := multipart.NewWriter(body)
	defer mp.Close()
	for key, val := range form {
		if strings.HasPrefix(val, "@") {
			val = val[1:]
			file, err := os.Open(val)
			if err != nil {
				return "", nil, err
			}
			defer file.Close()
			part, err := mp.CreateFormFile(key, val)
			if err != nil {
				return "", nil, err
			}
			_, err = io.Copy(part, file)
			if err != nil {
				log.Printf("Can't copy file %s to part %s. Error: %s", key, val, err)
			}
		} else {
			err := mp.WriteField(key, val)
			if err != nil {
				log.Printf("Can't write key %s with value %s to body. Error: %s", key, val, err)
			}
		}
	}
	return mp.FormDataContentType(), body, nil
}

func startTelegramSender() {
	telegramQueue = make(chan string, telegramQueueSize)

	go func() {
		for msg := range telegramQueue {
			attempt := 1
			for {
				if err := sendTelegramMessage(msg); err != nil {
					log.Printf("Can't send message to Telegram on attempt %d, retry after %s. Queue length: %d/%d. Error: %s", attempt, telegramRetryDelay, len(telegramQueue), cap(telegramQueue), err)
					attempt++
					time.Sleep(telegramRetryDelay)
					continue
				}

				if attempt > 1 {
					log.Printf("Telegram message sent after %d attempts", attempt)
				}
				break
			}
		}
	}()
}

func sendMessage(m string) {
	if telegramQueue == nil {
		if err := sendTelegramMessage(m); err != nil {
			log.Printf("Can't send message to Telegram. Error: %s", err)
		}
		return
	}

	select {
	case telegramQueue <- m:
	default:
		log.Printf("Telegram message queue is full (%d/%d), waiting for a free slot", len(telegramQueue), cap(telegramQueue))
		telegramQueue <- m
	}
}

func sendTelegramMessage(m string) error {
	apiURL := "https://api.telegram.org/" + config.Botid + ":" + config.Botkey + "/sendMessage"
	form := map[string]string{"disable_web_page_preview": "true", "parse_mode": "HTML", "chat_id": config.Chatid}
	if config.Chattype == "topic" {
		form["reply_to_message_id"] = config.Chattopic
	}

	form["text"] = m
	ct, body, err := createForm(form)
	if err != nil {
		return fmt.Errorf("create telegram message form: %w", err)
	}

	if config.Debug < 3 {
		log.Printf("Telegram API request with body: %s", body)
	}

	resp, err := telegramHTTPClient.Post(apiURL, ct, body)
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}
	defer resp.Body.Close()

	bodyText, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected telegram response %s: %s", resp.Status, string(bodyText))
	}

	return nil
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
	startTelegramSender()

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
			sendMessage(fmt.Sprintf("SMS from %s to %s :\n%s", html.EscapeString(src.String()), html.EscapeString(dst.String()), html.EscapeString(text)))
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
