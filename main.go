package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	//"github.com/ikawaha/kagome-dict/uni"
	//"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome-dict-ipa-neologd"
	"github.com/mattn/go-haiku"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	cliutil "github.com/bluesky-social/indigo/cmd/gosky/util"
	"github.com/bluesky-social/indigo/events"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/repomgr"
	"github.com/bluesky-social/indigo/xrpc"
	cid "github.com/ipfs/go-cid"

	"github.com/gorilla/websocket"
)

const name = "bsky-haikubot"

const version = "0.0.5"

var revision = "HEAD"

var (
	kagomeDic = ipaneologd.Dict()

	reLink     = regexp.MustCompile(`\b\w+://\S+\b`)
	reTag      = regexp.MustCompile(`\B#\S+`)
	reJapanese = regexp.MustCompile(`[０-９Ａ-Ｚａ-ｚぁ-ゖァ-ヾ一-鶴]`)

	//go:embed dict.json
	worddata []byte

	//go:embed bep-eng.dic
	engdata []byte

	words = map[*regexp.Regexp]string{}
)

type Event struct {
	schema string
	did    string
	rkey   string
	text   string
}

func init() {
	time.Local = time.FixedZone("Local", 9*60*60)

	var m map[string]string
	if err := json.Unmarshal(worddata, &m); err != nil {
		log.Fatal(err)
	}
	words = make(map[*regexp.Regexp]string)
	for k, v := range m {
		words[regexp.MustCompile(k)] = v
	}
	for _, v := range strings.Split(string(engdata), "\n") {
		if len(v) == 0 || v[0] == '#' {
			continue
		}
		tok := strings.Split(v, " ")
		words[regexp.MustCompile(`\b(?i:`+tok[0]+`)\b`)] = tok[1]
	}
}

func normalize(s string) string {
	s = reLink.ReplaceAllString(s, "")
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func isHaiku(s string) bool {
	for k, v := range words {
		s = k.ReplaceAllString(s, v)
	}
	return haiku.MatchWithOpt(s, []int{5, 7, 5}, &haiku.Opt{Udic: kagomeDic})
}

func isTanka(s string) bool {
	for k, v := range words {
		s = k.ReplaceAllString(s, v)
	}
	return haiku.MatchWithOpt(s, []int{5, 7, 5, 7, 7}, &haiku.Opt{Udic: kagomeDic})
}

func (bot *Bot) post(collection string, did string, rkey string, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	xrpcc, err := bot.makeXRPCC()
	if err != nil {
		return fmt.Errorf("cannot create client: %w", err)
	}

	getResp, err := comatproto.RepoGetRecord(context.TODO(), xrpcc, "", collection, did, rkey)
	if err != nil {
		return fmt.Errorf("cannot get record: %w", err)
	}

	post := &bsky.FeedPost{
		LexiconTypeID: "app.bsky.feed.post",
		Text:          text,
		CreatedAt:     time.Now().Local().Format(time.RFC3339),
		Embed: &bsky.FeedPost_Embed{
			EmbedRecord: &bsky.EmbedRecord{
				LexiconTypeID: "app.bsky.embed.record",
				Record:        &comatproto.RepoStrongRef{Cid: *getResp.Cid, Uri: getResp.Uri},
			},
		},
	}

	var lastErr error
	for retry := 0; retry < 3; retry++ {
		resp, err := comatproto.RepoCreateRecord(context.TODO(), xrpcc, &comatproto.RepoCreateRecord_Input{
			Collection: "app.bsky.feed.post",
			Repo:       xrpcc.Auth.Did,
			Record: &lexutil.LexiconTypeDecoder{
				Val: post,
			},
		})
		if err == nil {
			fmt.Println(resp.Uri)
			return nil
		}
		log.Printf("failed to create post: %v", err)
		lastErr = err
		time.Sleep(time.Second)
	}

	return fmt.Errorf("failed to create post: %w", lastErr)
}

func (bot *Bot) analyze(ev Event) error {
	if strings.Contains(ev.text, "#n575") || !reJapanese.MatchString(ev.text) {
		return nil
	}
	content := normalize(ev.text)
	if isHaiku(content) {
		log.Println("MATCHED HAIKU!", content)
		err := bot.post(ev.schema, ev.did, ev.rkey, content+" #n575 #haiku")
		if err != nil {
			return err
		}
	}
	if isTanka(content) {
		log.Println("MATCHED TANKA!", content)
		err := bot.post(ev.schema, ev.did, ev.rkey, content+" #n57577 #tanka")
		if err != nil {
			return err
		}
	}
	return nil
}

type Bot struct {
	Host     string
	Handle   string
	Password string
}

func (bot *Bot) makeXRPCC() (*xrpc.Client, error) {
	xrpcc := &xrpc.Client{
		Client: cliutil.NewHttpClient(),
		Host:   bot.Host,
		Auth:   &xrpc.AuthInfo{Handle: bot.Handle},
	}
	auth, err := comatproto.ServerCreateSession(context.TODO(), xrpcc, &comatproto.ServerCreateSession_Input{
		Identifier: xrpcc.Auth.Handle,
		Password:   bot.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create session: %w", err)
	}
	xrpcc.Auth.Did = auth.Did
	xrpcc.Auth.AccessJwt = auth.AccessJwt
	xrpcc.Auth.RefreshJwt = auth.RefreshJwt
	return xrpcc, nil
}

func getenv(name, def string) string {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	return s
}

func (bot *Bot) wssUrl() string {
	u, err := url.Parse(bot.Host)
	if err != nil {
		log.Fatal("invalid host", bot.Host)
	}
	return "wss://" + u.Host + "/xrpc/com.atproto.sync.subscribeRepos"
}

func run() error {
	var bot Bot
	bot.Host = getenv("HAIKUBOT_HOST", "https://bsky.social")
	bot.Handle = getenv("HAIKUBOT_HANDLE", "haiku.bsky.social")
	bot.Password = os.Getenv("HAIKUBOT_PASSWORD")

	if bot.Password == "" {
		log.Fatal("HAIKUBOT_PASSWORD is required")
	}

	con, _, err := websocket.DefaultDialer.Dial(bot.wssUrl(), http.Header{})
	if err != nil {
		return fmt.Errorf("dial failure: %w", err)
	}
	defer con.Close()

	q := make(chan Event, 100)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		retry := 0
	events_loop:
		for {
			select {
			case ev, ok := <-q:
				if !ok {
					break events_loop
				}
				if err := bot.analyze(ev); err != nil {
					log.Println(err)
				}
				retry = 0
			case <-time.After(10 * time.Second):
				retry++
				log.Println("Health check", retry)
				if retry > 60 {
					log.Println("timeout")
					con.Close()
					break events_loop
				}
			}
		}
	}()

	enc := json.NewEncoder(os.Stdout)
	events.ConsumeRepoStreamLite(context.Background(), con, func(op repomgr.EventKind, seq int64, path string, did string, rcid *cid.Cid, rec any) error {
		if op != "create" {
			return nil
		}
		post, ok := rec.(*bsky.FeedPost)
		if !ok {
			return nil
		}
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			return nil
		}
		enc.Encode(post)
		q <- Event{schema: parts[0], did: did, rkey: parts[1], text: post.Text}
		return nil
	})
	close(q)
	wg.Wait()

	return nil
}

func check(s string) int {
	s = normalize(s)
	fmt.Println(s)
	if isHaiku(s) {
		fmt.Println("HAIKU!")
		return 0
	} else if isTanka(s) {
		fmt.Println("TANKA!")
		return 0
	}
	return 1
}

func main() {
	var ver bool
	var tt bool
	flag.BoolVar(&ver, "v", false, "show version")
	flag.BoolVar(&tt, "t", false, "test")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	if tt {
		os.Exit(check(strings.Join(flag.Args(), " ")))
	}

	for {
		log.Println("start")
		if err := run(); err != nil {
			log.Println(err)
		}
	}
}
