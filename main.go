package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"regexp"
	rdebug "runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	//"github.com/ikawaha/kagome-dict/uni"
	//"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/mattn/go-haiku"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/util/cliutil"
	"github.com/bluesky-social/indigo/xrpc"

	"github.com/gorilla/websocket"
)

const name = "bsky-haikubot"

const version = "0.0.62"

var revision = "HEAD"

var (
	reLink          = regexp.MustCompile(`^\w+://\S+$`)
	reTag           = regexp.MustCompile(`^#\S+$`)
	reJapanese      = regexp.MustCompile(`[０-９Ａ-Ｚａ-ｚぁ-ゖァ-ヾ一-鶴]`)
	reHiragKataOnly = regexp.MustCompile(`^([ぁ-ゖー]+|[ァ-ヾー]+)$`)

	tagRe = regexp.MustCompile(`\B#\S+`)

	debug = false

	kagomeDic *dict.Dict

	//go:embed userdic.txt
	dicdata []byte

	userDic *dict.UserDict

	followers map[string]struct{}

	mu sync.Mutex
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

type entry struct {
	start int64
	end   int64
	text  string
}

func extractTagsBytes(text string) []entry {
	var result []entry
	matches := tagRe.FindAllStringSubmatchIndex(text, -1)
	for _, m := range matches {
		result = append(result, entry{
			text:  strings.TrimPrefix(text[m[0]:m[1]], "#"),
			start: int64(len(text[0:m[0]])),
			end:   int64(len(text[0:m[1]]))},
		)
	}
	return result
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
		Langs: []string{"ja"},
	}

	for _, entry := range extractTagsBytes(text) {
		post.Facets = append(post.Facets, &bsky.RichtextFacet{
			Features: []*bsky.RichtextFacet_Features_Elem{
				{
					RichtextFacet_Tag: &bsky.RichtextFacet_Tag{
						Tag: entry.text,
					},
				},
			},
			Index: &bsky.RichtextFacet_ByteSlice{
				ByteStart: entry.start,
				ByteEnd:   entry.end,
			},
		})
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
		slog.Error("failed to create post", slog.Any("error", err))
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
	if !bot.hasRelation(ev.did) {
		return nil
	}
	content := normalize(ev.text)
	clen := utf8.RuneCountInString(content)
	if clen < 10 || clen > 100 {
		return nil
	}
	if isHaiku(content) {
		slog.Info("MATCHED HAIKU!", slog.String("content", content))
		err := bot.post(ev.schema, ev.did, ev.rkey, content+" #n575 #haiku")
		if err != nil {
			return err
		}
	}
	if isTanka(content) {
		slog.Info("MATCHED TANKA!", slog.String("content", content))
		err := bot.post(ev.schema, ev.did, ev.rkey, content+" #n57577 #tanka")
		if err != nil {
			return err
		}
	}
	return nil
}

type Bot struct {
	Jetstream string
	Host      string
	Handle    string
	Password  string
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

func (bot *Bot) updateFollowers() {
	defer func() {
		err := recover()
		if err != nil {
			slog.Error("recover", slog.Any("error", err))
		}
	}()

	xrpcc, err := bot.makeXRPCC()
	if err != nil {
		slog.Error("cannot create client", slog.Any("error", err))
		return
	}

	var cursor string
	ffs := map[string]struct{}{}
	for {
		fws, err := bsky.GraphGetFollowers(context.TODO(), xrpcc, xrpcc.Auth.Handle, cursor, 100)
		if err != nil {
			slog.Error("failed to get record", slog.Any("error", err))
			return
		}

		for _, f := range fws.Followers {
			ffs[f.Did] = struct{}{}
		}
		if fws.Cursor == nil {
			break
		}
		cursor = *fws.Cursor
	}

	mu.Lock()
	followers = ffs
	mu.Unlock()
}

func (bot *Bot) hasRelation(did string) bool {
	mu.Lock()
	defer mu.Unlock()

	_, ok := followers[did]
	return ok
}

func getenv(name, def string) string {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	return s
}

func (bot *Bot) jetstreamURL(cursor int64) string {
	u, err := url.Parse(bot.Jetstream)
	if err != nil {
		slog.Error("invalid jetstream url", slog.String("url", bot.Jetstream))
		return bot.Jetstream
	}
	q := u.Query()
	q.Set("wantedCollections", "app.bsky.feed.post")
	if cursor > 0 {
		q.Set("cursor", fmt.Sprint(cursor))
	} else {
		q.Del("cursor")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func heartbeatPush(url string) {
	resp, err := http.Get(url)
	if err != nil {
		slog.Error("failed to post heartbeat", slog.Any("error", err))
		return
	}
	defer resp.Body.Close()
}

// jetstreamEvent is a message of Jetstream. only the fields used by the bot
// are decoded. see https://github.com/bluesky-social/jetstream
type jetstreamEvent struct {
	Did    string `json:"did"`
	TimeUS int64  `json:"time_us"`
	Kind   string `json:"kind"`
	Commit *struct {
		Operation  string `json:"operation"`
		Collection string `json:"collection"`
		Rkey       string `json:"rkey"`
		Record     struct {
			Text  string   `json:"text"`
			Langs []string `json:"langs"`
		} `json:"record"`
	} `json:"commit"`
}

func hasJa(langs []string) bool {
	if len(langs) == 0 {
		return true
	}
	for _, lang := range langs {
		if lang == "ja" {
			return true
		}
	}
	return false
}

// run reads posts from Jetstream until the connection fails. cursor holds
// time_us of the last message received, so that the next run resumes from
// there.
func run(cursor *int64) error {
	var bot Bot
	bot.Jetstream = getenv("HAIKUBOT_JETSTREAM", "wss://jetstream2.us-east.bsky.network/subscribe")
	bot.Host = getenv("HAIKUBOT_HOST", "https://bsky.social")
	bot.Handle = getenv("HAIKUBOT_HANDLE", "haiku.bsky.social")
	bot.Password = os.Getenv("HAIKUBOT_PASSWORD")

	if bot.Password == "" {
		panic("HAIKUBOT_PASSWORD is required")
	}

	go bot.updateFollowers()

	from := *cursor
	if from > 0 {
		from++
	}
	con, _, err := websocket.DefaultDialer.Dial(bot.jetstreamURL(from), http.Header{})
	if err != nil {
		return fmt.Errorf("dial failure: %w", err)
	}
	defer con.Close()

	q := make(chan Event, 100)

	hbtimer := time.NewTicker(5 * time.Minute)
	defer hbtimer.Stop()

	uftimer := time.NewTicker(5 * time.Minute)
	defer uftimer.Stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case ev, ok := <-q:
				if !ok {
					return
				}
				if err := bot.analyze(ev); err != nil {
					slog.Error("failed to analyze", slog.Any("error", err))
				}
			case <-hbtimer.C:
				if url := os.Getenv("HEARTBEAT_URL"); url != "" {
					go heartbeatPush(url)
				}
			case <-uftimer.C:
				go bot.updateFollowers()
			}
		}
	}()

	enc := json.NewEncoder(os.Stdout)

	for {
		// posts of the whole network keep coming, so a quiet connection is
		// a dead one.
		con.SetReadDeadline(time.Now().Add(time.Minute))
		_, b, err := con.ReadMessage()
		if err != nil {
			err = fmt.Errorf("failed to read: %w", err)
			close(q)
			wg.Wait()
			return err
		}
		var ev jetstreamEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			slog.Error("failed to decode", slog.Any("error", err))
			continue
		}
		if ev.TimeUS > *cursor {
			*cursor = ev.TimeUS
		}
		if ev.Kind != "commit" || ev.Commit == nil || ev.Commit.Operation != "create" || ev.Commit.Collection != "app.bsky.feed.post" {
			continue
		}
		if blocklisted(ev.Did) || !bot.hasRelation(ev.Did) {
			continue
		}
		post := &ev.Commit.Record
		if !hasJa(post.Langs) {
			continue
		}
		if strings.Contains(post.Text, "#n575") || !reJapanese.MatchString(post.Text) {
			continue
		}
		if reHiragKataOnly.MatchString(post.Text) {
			continue
		}
		if debug {
			enc.Encode(post)
		}

		q <- Event{schema: ev.Commit.Collection, did: ev.Did, rkey: ev.Commit.Rkey, text: post.Text}
	}
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

	kagomeDic = systemDict()
	// the dictionary allocates far more than it keeps while loading. give
	// the rest back to the OS instead of letting it sit in the heap.
	rdebug.FreeOSMemory()

	if tt {
		os.Exit(check(strings.Join(flag.Args(), " ")))
	}

	go http.ListenAndServe("0.0.0.0:6060", nil)

	var cursor int64
	for {
		slog.Info("start")
		if err := run(&cursor); err != nil {
			slog.Error("failed to run", slog.Any("error", err))
		}
		time.Sleep(time.Second)
	}
}
