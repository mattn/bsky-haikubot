package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	//"github.com/ikawaha/kagome-dict/uni"
	//"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome-dict-ipa-neologd"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/mattn/go-haiku"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/repo"
	"github.com/bluesky-social/indigo/repomgr"
	"github.com/bluesky-social/indigo/util/cliutil"
	"github.com/bluesky-social/indigo/xrpc"

	"github.com/gorilla/websocket"
)

const name = "bsky-haikubot"

const version = "0.0.36"

var revision = "HEAD"

var (
	reLink     = regexp.MustCompile(`^\w+://\S+$`)
	reTag      = regexp.MustCompile(`^#\S+$`)
	reJapanese = regexp.MustCompile(`[０-９Ａ-Ｚａ-ｚぁ-ゖァ-ヾ一-鶴]`)

	debug = false

	kagomeDic = ipaneologd.Dict()

	//go:embed userdic.txt
	dicdata []byte

	userDic *dict.UserDict
)

type Event struct {
	schema string
	did    string
	rkey   string
	text   string
}

func init() {
	time.Local = time.FixedZone("Local", 9*60*60)

	r, err := dict.NewUserDicRecords(bytes.NewReader(dicdata))
	if err != nil {
		panic(err.Error())
	}
	userDic, err = r.NewUserDict()
	if err != nil {
		panic(err.Error())
	}
}

func normalize(s string) string {
	result := ""
	for _, word := range strings.Fields(s) {
		if reLink.MatchString(word) || reTag.MatchString(word) {
			continue
		}
		result += " " + word
	}
	return strings.TrimSpace(result)
}

func isHaiku(s string) bool {
	return haiku.MatchWithOpt(s, []int{5, 7, 5}, &haiku.Opt{Dict: kagomeDic, UserDict: userDic, Debug: debug})
}

func isTanka(s string) bool {
	return haiku.MatchWithOpt(s, []int{5, 7, 5, 7, 7}, &haiku.Opt{Dict: kagomeDic, UserDict: userDic, Debug: debug})
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

func blocklisted(did string) bool {
	var blocklist = []string{
		"did:plc:7n2uogskixiouu4ofz3o4vdf",
		"did:plc:dxx5meybbce2bhqxxviivwhm",
		"did:plc:bb5yxpnjg3ev7zuh7sdg43s6",
	}
	for _, block := range blocklist {
		if did == block {
			return true
		}
	}
	return false
}

func (bot *Bot) analyze(ev Event) error {
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
	Bgs      string
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
	u, err := url.Parse(bot.Bgs)
	if err != nil {
		log.Fatal("invalid host", bot.Host)
	}
	return "wss://" + u.Host + "/xrpc/com.atproto.sync.subscribeRepos"
}

func heartbeatPush(url string) {
	resp, err := http.Get(url)
	if err != nil {
		log.Println(err.Error())
	}
	defer resp.Body.Close()
}

func run() error {
	var bot Bot
	bot.Bgs = getenv("HAIKUBOT_BGS", "https://bsky.network")
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

	hbtimer := time.NewTicker(5 * time.Minute)
	defer hbtimer.Stop()

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
			case <-hbtimer.C:
				if url := os.Getenv("HEARTBEAT_URL"); url != "" {
					go heartbeatPush(url)
				}
			case <-time.After(10 * time.Second):
				retry++
				log.Println("Health check", retry)
				if retry > 60 {
					log.Println("timeout")
					con.Close()
					break events_loop
				}
				runtime.GC()
			}
		}
	}()

	enc := json.NewEncoder(os.Stdout)

	ctx := context.Background()
	rsc := &events.RepoStreamCallbacks{
		RepoCommit: func(evt *comatproto.SyncSubscribeRepos_Commit) error {
			if evt.TooBig {
				log.Printf("skipping too big events for now: %d", evt.Seq)
				return nil
			}
			r, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(evt.Blocks))
			if err != nil {
				return fmt.Errorf("reading repo from car (seq: %d, len: %d): %w", evt.Seq, len(evt.Blocks), err)
			}

			for _, op := range evt.Ops {
				ek := repomgr.EventKind(op.Action)
				switch ek {
				case repomgr.EvtKindCreateRecord, repomgr.EvtKindUpdateRecord:
					rc, rec, err := r.GetRecord(ctx, op.Path)
					if err != nil {
						e := fmt.Errorf("getting record %s (%s) within seq %d for %s: %w", op.Path, *op.Cid, evt.Seq, evt.Repo, err)
						log.Print(e)
						continue
					}

					if lexutil.LexLink(rc) != *op.Cid {
						return fmt.Errorf("mismatch in record and op cid: %s != %s", rc, *op.Cid)
					}

					if ek != "create" {
						return nil
					}
					post, ok := rec.(*bsky.FeedPost)
					if !ok {
						return nil
					}
					if len(post.Langs) > 0 {
						hasJa := false
						for _, lang := range post.Langs {
							if lang == "ja" {
								hasJa = true
								break
							}
						}
						if !hasJa {
							return nil
						}
					}
					if strings.Contains(post.Text, "#n575") || !reJapanese.MatchString(post.Text) {
						return nil
					}
					if blocklisted(evt.Repo) {
						log.Println("BLOCKED ", evt.Repo)
						return nil
					}
					parts := strings.Split(op.Path, "/")
					if len(parts) < 2 {
						return nil
					}
					enc.Encode(post)

					q <- Event{schema: parts[0], did: evt.Repo, rkey: parts[1], text: post.Text}
				}
			}
			return nil
		},
		RepoInfo: func(info *comatproto.SyncSubscribeRepos_Info) error {
			return nil
		},
		Error: func(errf *events.ErrorFrame) error {
			return fmt.Errorf("error frame: %s: %s", errf.Error, errf.Message)
		},
	}
	err = events.HandleRepoStream(ctx, con, sequential.NewScheduler("stream", rsc.EventHandler))
	if err != nil {
		log.Println(err)
	}
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
	flag.BoolVar(&debug, "V", false, "verbose")
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

	go http.ListenAndServe("0.0.0.0:6060", nil)

	for {
		log.Println("start")
		if err := run(); err != nil {
			log.Println(err)
		}
	}
}
