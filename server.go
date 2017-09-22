package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/PuerkitoBio/goquery"
	"github.com/go-xorm/xorm"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"github.com/patrickmn/go-cache"
	"html"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Domain   string
	Motd     string
	DbDriver string
	DbDsn    string
	Listen   string

	AccountCacheTtl time.Duration
	TweetCacheTtl   time.Duration
}

type Stats struct {
	StartTime     time.Time
	DbAccountMiss int
	DbAccountHit  int
	DbTweetMiss   int
	DbTweetHit    int
	DbTweets      int
	DbAccounts    int
}

func (s Stats) DbAccountHitRatio() float32 {
	return float32(s.DbAccountHit) / float32(math.Max(float64(s.DbAccountMiss), 1))
}
func (s Stats) DbTweetHitRatio() float32 {
	return float32(s.DbTweetHit) / float32(math.Max(float64(s.DbTweetMiss), 1))
}
func (s Stats) Uptime() time.Duration {
	return time.Now().UTC().Sub(s.StartTime).Truncate(time.Second)
}

type InstanceCtx struct {
	Domain string
	Cache  *cache.Cache
	DB     *xorm.Engine
	Stats  *Stats
	Config Config
}

func user_url(inst InstanceCtx, u LocalAccount) string {
	return fmt.Sprintf("https://%s/%s", inst.Domain, u.Name)
}
func user_res_url(inst InstanceCtx, u LocalAccount, res string) string {
	return fmt.Sprintf("https://%s/%s/%s", inst.Domain, u.Name, res)
}
func status_url(inst InstanceCtx, u LocalAccount, s LocalTweet) string {
	return fmt.Sprintf("https://%s/%s/status/%d", inst.Domain, u.Name, s.TweetId)
}

type UserStub struct {
	Name string
}

type Attachment struct {
	Url  string
	Type string
}

func parse_content(inst InstanceCtx, s *goquery.Selection, mentions *[]UserStub) string {
	text_parts := s.Contents().Map(func(i int, node *goquery.Selection) string {
		if goquery.NodeName(node) == "#text" {
			return html.EscapeString(node.Text())
		} else if goquery.NodeName(node) == "img" {
			return html.EscapeString(node.AttrOr("alt", ""))
		} else if goquery.NodeName(node) == "a" {
			var b bytes.Buffer

			if node.HasClass("twitter-atreply") {
				name := node.Find("b").First().Text()
				t := template.Must(template.New("").Parse("<span class=\"h-card\"><a href=\"{{.Url}}\" class=\"u-url mention\">@<span>{{.Name}}</span></a></span>"))
				t.Execute(&b, map[string]interface{}{
					"Url":  fmt.Sprintf("https://%s/%s", inst.Domain, name),
					"Name": name,
				})
				*mentions = append(*mentions, UserStub{Name: name})
			} else {
				t := template.Must(template.New("").Parse("<a href=\"{{.Url}}\">{{.Url}}</a>"))
				t.Execute(&b, map[string]interface{}{
					"Url": node.AttrOr("data-expanded-url", node.AttrOr("href", "")),
				})
			}
			return b.String()
		}
		return html.EscapeString(node.Text())
	})
	return "<p>" + strings.Join(text_parts, " ") + "</p>"
}

func find_attachments(inst InstanceCtx, s *goquery.Selection) []Attachment {
	var r []Attachment
	s.Find(".AdaptiveMedia-photoContainer").Each(func(i int, node *goquery.Selection) {
		url := node.AttrOr("data-image-url", "")
		lurl := strings.ToLower(url)
		var t string
		switch {
		case strings.HasSuffix(lurl, ".jpg") || strings.HasSuffix(lurl, ".jpeg"):
			t = "image/jpeg"
		case strings.HasSuffix(lurl, ".png"):
			t = "image/png"
		}
		r = append(r, Attachment{Url: url, Type: t})
	})
	return r
}

func parse_timestamp(s string) time.Time {
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("%s", err)
		return time.Now().UTC()
	}
	t := time.Unix(int64(n), 0).UTC()
	return t
}

func query(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Accept-Language", "en-GB,en;q=0.8,en-US;q=0.6,fr;q=0.4")
	req.Header.Set("Cookie", "lang=en-gb")
	cookie := http.Cookie{Name: "lang", Value: "en-gb"}
	req.AddCookie(&cookie)

	client := &http.Client{}
	resp, err := client.Do(req)
	return resp, err
}

func get_recent_statuses(inst InstanceCtx, user LocalAccount) *[]LocalTweet {
	if user.RecentStatuses != nil {
		return user.RecentStatuses
	}

	var statuses []LocalTweet
	err := inst.DB.Sql("select * from local_tweet where local_account_id=? order by published_time desc limit 20", user.LocalId).Find(&statuses)
	if err != nil {
		log.Printf("db error: %s", err)
		return nil
	}
	user.RecentStatuses = &statuses
	return user.RecentStatuses
}

func tw_get_user(inst InstanceCtx, name string) (*LocalAccount, error) {
	ttl := 30 * time.Minute
	tweet_ttl := 30 * time.Minute

	twitter_url := fmt.Sprintf("https://twitter.com/%s", name)
	var user LocalAccount

	name = strings.ToLower(name)

	// Query database
	db_hit, err := inst.DB.Sql("select * from local_account where name=?", name).Get(&user)
	if err != nil {
		log.Fatalf("db error: %s", err.Error())
	}
	if db_hit && user.LastUpdate.After(time.Now().UTC().Add(-ttl)) {
		inst.Stats.DbAccountHit++
		log.Printf("tw_get_user(%s): db hit (%d)", name, user.LocalId)
		return &user, nil
	}

	inst.Stats.DbAccountMiss++
	log.Printf("tw_get_user(%s): db miss", name)

	resp, err := http.Get(twitter_url)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error querying page: %s", err))
	}

	flated, err := zlib.NewReader(resp.Body)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error parsing page: %s", err))
	}
	doc, err := goquery.NewDocumentFromReader(flated)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error parsing page: %s", err))
	}

	username := doc.Find(".ProfileSidebar .username b").Text()

	user.Name = username
	user.DisplayName = doc.Find(".ProfileHeaderCard-name a").Text()
	user.AvatarUrl = doc.Find(".ProfileAvatar-image").AttrOr("src", "")
	user.Bio = doc.Find(".ProfileHeaderCard-bio").Text()
	user.LastUpdate = time.Now().UTC()

	var statuses []LocalTweet
	doc.Find(".tweet").Each(func(i int, s *goquery.Selection) {
		str_id, _ := s.Attr("data-tweet-id")
		_id, err := strconv.ParseInt(str_id, 10, 64)
		if err != nil {
			log.Printf("error reading tweet id: %s: %s", str_id, err)
			return
		}

		id := uint64(_id)
		tweet := LocalTweet{TweetId: id}

		if id > user.LastTweetId {
			user.LastTweetId = id
		}

		tweet_hit, err := inst.DB.Sql("select * from local_tweet where tweet_id=?", id).Get(&tweet)
		if err != nil {
			log.Fatalf("db error: %s", err.Error())
		}
		if tweet_hit && tweet.LastUpdate.After(time.Now().UTC().Add(-tweet_ttl)) {
			statuses = append(statuses, tweet)
			return
		}

		tweet_username := s.Find(".content .account-group .username b").Text()

		if tweet_username == user.Name {
			tweet.LocalAccountId = user.LocalId
			tweet.IsBoost = false
		} else {
			tweet.IsBoost = true

			var su_account LocalAccount
			su_hit, err := inst.DB.Sql("select local_id from local_account where name=?", tweet_username).Get(&su_account)
			if err != nil {
				log.Fatalf("db error: %s", err.Error())
			}
			if !su_hit {
				su_account.Name = tweet_username
				su_account.DisplayName = s.Find(".content .account-group .FullNameGroup strong").Text()
				su_account.AvatarUrl = doc.Find(".tweet.js-original-tweet .avatar").AttrOr("src", "")
				inst.DB.Insert(&su_account)
				// note that we don't set LastUpdate, as it is incomplete
			}
			tweet.LocalAccountId = su_account.LocalId
		}

		tweet.Content = parse_content(inst, s.Find(".content .tweet-text"), &tweet.Mentions)
		tweet.PublishTime = parse_timestamp(s.Find(".js-short-timestamp").AttrOr("data-time", ""))
		tweet.ConversationId = s.AttrOr("data-conversation-id", "")
		tweet.Attachments = find_attachments(inst, s)

		if tweet_hit {
			inst.DB.Update(tweet, LocalTweet{TweetId: id})
		} else {
			inst.DB.Insert(tweet)
		}

		statuses = append(statuses, tweet)
	})
	user.RecentStatuses = &statuses

	if db_hit {
		inst.DB.Update(&user, LocalAccount{LocalId: user.LocalId})
	} else {
		inst.DB.Insert(&user)
	}

	return &user, nil
}

func tw_get_status(inst InstanceCtx, username string, id string) (*LocalTweet, error) {
	ttl := 30 * time.Minute
	twitter_url := fmt.Sprintf("https://twitter.com/%s/status/%s", username, id)

	var tweet LocalTweet

	// Query database
	db_hit, err := inst.DB.Sql("select * from local_tweet where tweet_id=?", id).Get(&tweet)
	if err != nil {
		log.Fatalf("db error: %s", err.Error())
	}
	if db_hit && tweet.LastUpdate.After(time.Now().UTC().Add(-ttl)) {
		inst.Stats.DbTweetHit++
		log.Printf("tw_get_status(%s, %s): db hit", username, id)
		return &tweet, nil
	}

	inst.Stats.DbTweetMiss++
	log.Printf("tw_get_status(%s, %s): db miss", username, id)

	resp, err := http.Get(twitter_url)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error querying page: %s", err))
	}

	flated, err := zlib.NewReader(resp.Body)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error parsing page: %s", err))
	}
	doc, err := goquery.NewDocumentFromReader(flated)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error parsing page: %s", err))
	}

	id_i, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error reading tweet id: %s: %s", id, err))
	}

	tweet.Content = parse_content(inst, doc.Find(".tweet.js-original-tweet .tweet-text"), &tweet.Mentions)
	tweet.TweetId = uint64(id_i)
	tweet.PublishTime = parse_timestamp(doc.Find(".tweet.js-original-tweet .tweet-timestamp span").AttrOr("data-time", ""))
	tweet.Retweets, _ = strconv.Atoi(doc.Find(".stats .js-stat-retweets a strong").Text())
	tweet.Favorites, _ = strconv.Atoi(doc.Find(".stats .js-stat-favorites strong").Text())
	tweet.ConversationId = doc.Find(".tweet.js-original-tweet").AttrOr("data-conversation-id", "")
	tweet.LastUpdate = time.Now().UTC()
	tweet.Attachments = find_attachments(inst, doc.Find(".tweet.js-original-tweet"))

	username = doc.Find(".tweet.js-original-tweet").AttrOr("data-screen-name", "")
	if strings.HasPrefix(username, "@") {
		username = username[1:]
	}

	var user LocalAccount
	su_hit, err := inst.DB.Sql("select local_id from local_account where name=?", username).Get(&user)
	if !su_hit {
		user.Name = username
		user.DisplayName = doc.Find(".tweet.js-original-tweet").AttrOr("data-name", "")
		user.AvatarUrl = doc.Find(".tweet.js-original-tweet .avatar").AttrOr("src", "")
		inst.DB.Insert(&user)
		// note that we don't set LastUpdate, as it is incomplete
	}
	tweet.LocalAccountId = user.LocalId

	if doc.Find(".tweet.js-original-tweet").AttrOr("data-has-parent-tweet", "") == "true" {
		r := doc.Find(".in-reply-to .tweet").Last()
		r_name := r.AttrOr("data-screen-name", "")
		r_id := r.AttrOr("data-tweet-id", "")
		url := fmt.Sprintf("https://%s/%s/status/%s", inst.Domain, r_name, r_id)
		tweet.ReplyToUrl = &url
	}

	if db_hit {
		inst.DB.Update(&tweet, LocalTweet{TweetId: tweet.TweetId})
	} else {
		inst.DB.Insert(&tweet)
	}

	return &tweet, nil
}

func webfinger_link(rel string, type_ string, href string) map[string]interface{} {
	r := make(map[string]interface{})
	r["rel"] = rel
	r["type"] = type_
	r["href"] = href
	return r
}
func webfinger_acct(inst InstanceCtx, username string) map[string]interface{} {
	r := make(map[string]interface{})

	//gw_url := fmt.Sprintf("https://%s/%s.atom", inst.Domain, username)
	twitter_url := fmt.Sprintf("https://twitter.com/%s", username)
	ap_url := fmt.Sprintf("https://%s/%s", inst.Domain, username)

	r["subject"] = "acct:" + username + "@" + inst.Domain
	r["aliases"] = []string{
		ap_url,
		twitter_url,
	}
	r["links"] = []map[string]interface{}{
		webfinger_link("http://webfinger.net/rel/profile-page", "text/html", twitter_url),
		//webfinger_link("http://schemas.google.com/g/2010#updates-from", "application/atom+xml", gw_url),
		webfinger_link("self", "application/activity+json", ap_url),
	}
	return r
}

type APTag struct {
	Type string `json:"type"`
	Href string `json:"href"`
	Name string `json:"name"`
}

type APOutbox struct {
	Context      []interface{} `json:"@context"`
	Id           string        `json:"id"`
	Type         string        `json:"type"`
	TotalItems   int           `json:"totalItems"`
	OrderedItems []APActivity  `json:"orderedItems"`
}

type APNote struct {
	Context          []interface{}  `json:"@context"`
	Id               string         `json:"id"`
	Type             string         `json:"type"`
	Summary          *string        `json:"summary"`
	Content          string         `json:"content"`
	InReplyTo        *string        `json:"inReplyTo"`
	Published        time.Time      `json:"published"`
	Url              string         `json:"url"`
	AttributedTo     string         `json:"attributedTo"`
	To               []string       `json:"to"`
	Cc               []string       `json:"cc"`
	Sensitive        bool           `json:"sensitive"`
	AtomURI          string         `json:"atomUri"`
	InReplyToAtomURI *string        `json:"inReplyToAtomUri"`
	Conversation     string         `json:"conversation"`
	Attachment       []APAttachment `json:"attachment"`
	Tag              []APTag        `json:"tag"`
}

type APActivity struct {
	Context   []interface{} `json:"@context"`
	Id        string        `json:"id"`
	Type      string        `json:"type"`
	Actor     string        `json:"actor"`
	To        []string      `json:"to"`
	Cc        []string      `json:"cc"`
	Object    interface{}   `json:"object"`
	Signature interface{}   `json:"signature"`
}
type APIcon struct {
	Type string `json:"type"`
	Url  string `json:"url"`
}
type APPublicKey struct {
	Id           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPem string `json:"publicKeyPem"`
}
type APEndpoint struct {
	SharedInbox string `json:"sharedInbox"`
}

type APPerson struct {
	Context                   []interface{} `json:"@context"`
	Id                        string        `json:"id"`
	Type                      string        `json:"type"`
	Following                 string        `json:"following"`
	Followers                 string        `json:"followers"`
	Inbox                     string        `json:"inbox"`
	Outbox                    string        `json:"outbox"`
	PreferredUsername         string        `json:"preferredUsername"`
	Name                      string        `json:"name"`
	Summary                   string        `json:"summary"`
	Url                       string        `json:"url"`
	ManuallyApprovesFollowers bool          `json:"manuallyApprovesFollowers"`
	PublicKey                 APPublicKey   `json:"publicKey"`
	Endpoints                 APEndpoint    `json:"endpoints"`
	Icon                      APIcon        `json:"icon"`
}

type APAttachment struct {
	Type      string `json:"type"`
	MediaType string `json:"mediaType"`
	Url       string `json:"url"`
}

func ap_person(inst InstanceCtx, user LocalAccount) APPerson {
	o := APPerson{
		Id:                user_url(inst, user),
		Type:              "Person",
		Following:         user_res_url(inst, user, "followings"),
		Followers:         user_res_url(inst, user, "followers"),
		Inbox:             user_res_url(inst, user, "inbox"),
		Outbox:            user_res_url(inst, user, "outbox"),
		PreferredUsername: user.Name,
		Name:              user.DisplayName,
		Summary:           user.Bio,
		Url:               user_url(inst, user),
		PublicKey:         APPublicKey{},
		Endpoints:         APEndpoint{},
		Icon: APIcon{
			Type: "Image",
			Url:  user.AvatarUrl,
		},
		ManuallyApprovesFollowers: false,
	}
	return o
}

func ap_note(inst InstanceCtx, user LocalAccount, status LocalTweet) APNote {
	o := APNote{
		Id:           status_url(inst, user, status),
		Type:         "Note",
		Summary:      nil,
		Content:      status.Content,
		Published:    status.PublishTime,
		Url:          status_url(inst, user, status),
		AttributedTo: user_url(inst, user),
		To:           []string{"https://www.w3.org/ns/activitystreams#Public"},
		Cc:           []string{user_res_url(inst, user, "followers")},
		Sensitive:    false,
		AtomURI:      status_url(inst, user, status) + ".atom",
		Attachment:   []APAttachment{},
		Conversation: status.ConversationId,
		Tag:          []APTag{},
	}
	for _, a := range status.Attachments {
		o.Attachment = append(o.Attachment, APAttachment{
			Type:      "Document",
			Url:       a.Url,
			MediaType: a.Type,
		})
	}
	if status.ReplyToUrl != nil {
		u := *status.ReplyToUrl + ".atom"
		o.InReplyTo = status.ReplyToUrl
		o.InReplyToAtomURI = &u
	}
	for _, mention := range status.Mentions {
		url := fmt.Sprintf("https://%s/%s", inst.Domain, mention.Name)
		o.Tag = append(o.Tag, APTag{
			Type: "Mention",
			Name: "@" + mention.Name + "@" + inst.Domain,
			Href: url,
		})
		o.Cc = append(o.Cc, url)
	}
	return o
}

func ap_activity(inst InstanceCtx, user LocalAccount, status LocalTweet) APActivity {
	note := ap_note(inst, user, status)
	o := APActivity{
		Id:     status_url(inst, user, status),
		Type:   "Create",
		Actor:  user_url(inst, user),
		To:     note.To,
		Cc:     note.Cc,
		Object: note,
	}
	return o
}

func ap_context() []interface{} {
	return []interface{}{
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
		map[string]interface{}{
			"manuallyApprovesFollowers": "as:manuallyApprovesFollowers",
			"sensitive":                 "as:sensitive",
			"hashtag":                   "as:Hashtag",
			"ostatus":                   "http://ostatus.org#",
			"atomUri":                   "ostatus:atomUri",
			"inReplyToAtomUri":          "ostatus:inReplyToAtomUri",
			"conversation":              "ostatus:conversation",
		},
	}
}

func ap_outbox(inst InstanceCtx, user LocalAccount) APOutbox {
	var activities []APActivity
	statuses := get_recent_statuses(inst, user)
	if statuses != nil {
		for _, a := range *statuses {
			var status_user LocalAccount
			if a.LocalAccountId == user.LocalId {
				status_user = user
			} else {
				su_hit, err := inst.DB.Sql("select local_id from local_account where local_id=?", a.LocalAccountId).Get(&status_user)
				if err != nil {
					log.Fatalf("db error: %s", err.Error())
				}
				if !su_hit {
					continue
				}
			}
			activities = append(activities, ap_activity(inst, user, a))
		}
	}
	return APOutbox{
		Id:           user_res_url(inst, user, "outbox"),
		Type:         "OrderedCollection",
		TotalItems:   len(activities),
		OrderedItems: activities,
	}
}

func any_in_array(array []string, search_any []string) bool {
	// why the fuck do i have to write this
	// i miss generics and functional programming
	for _, search := range search_any {
		for _, v := range array {
			if v == search {
				return true
			}
		}
	}
	return false
}

func ap_check_headers(r *http.Request) error {
	cts := []string{
		"application/ld+json; profile=\"https://www.w3.org/ns/activitystreams\"",
		"application/activity+json",
	}

	accept := r.Header.Get("accept")

	var q_cts []string

	for _, v := range strings.Split(accept, ",") {
		q_cts = append(q_cts, strings.TrimSpace(v))
	}
	if !any_in_array(cts, q_cts) {
		return errors.New(fmt.Sprintf("unexpected type: %s", accept))
	}
	return nil
}

func Log(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func handle_reload(inst *InstanceCtx, c chan os.Signal) {
	for {
		<-c

		if _, err := toml.DecodeFile(*config_path, &inst.Config); err != nil {
			log.Printf("failed to open config file %s: %s", *config_path, err)
		}
		log.Printf("reloaded config file: %s", *config_path)

		inst.Domain = inst.Config.Domain
	}
}

// Locally stored data

type LocalAccount struct {
	LocalId     uint64 `xorm:"pk autoincr"`
	Name        string `xorm:"unique index not null"`
	DisplayName string
	Bio         string
	AvatarUrl   string

	LastTweetId uint64
	LastUpdate  time.Time
	FirstUpdate time.Time `xorm:"not null"`

	RecentStatuses *[]LocalTweet `xorm:"-"`
}

type LocalTweet struct {
	TweetId        uint64 `xorm:"pk"`
	LocalAccountId uint64 `xorm:"index not null"`
	Content        string

	PublishTime    time.Time
	Retweets       int
	Favorites      int
	IsBoost        bool
	ReplyToUrl     *string
	ConversationId string
	Mentions       []UserStub
	Attachments    []Attachment

	LastUpdate  time.Time
	FirstUpdate time.Time `xorm:"not null"`
}

var config_path = flag.String("c", "./twitterpub.toml", "Configuration file path")

func custom_usage() {
	fmt.Printf("TwitterPub - a minimal ActivityPub gateway to Twitter\n")
	fmt.Printf("usage: %s [-c config.toml]\n", os.Args[0])
	flag.PrintDefaults()
}

func update_stats(inst InstanceCtx) {
	inst.DB.Sql("select count(*) from local_account").Get(&inst.Stats.DbAccounts)
	inst.DB.Sql("select count(*) from local_tweet").Get(&inst.Stats.DbTweets)
}

func main() {
	flag.Usage = custom_usage

	inst := InstanceCtx{
		Config: Config{
			DbDriver:        "sqlite3",
			DbDsn:           "./database.sql",
			Listen:          ":8000",
			AccountCacheTtl: 30 * time.Minute,
			TweetCacheTtl:   30 * time.Minute,
		},
		Stats: new(Stats),
	}
	inst.Stats.StartTime = time.Now().UTC()

	flag.Parse()

	if _, err := toml.DecodeFile(*config_path, &inst.Config); err != nil {
		log.Printf("failed to open config file %s: %s", *config_path, err)
	}

	inst.Domain = inst.Config.Domain
	var err error
	inst.DB, err = xorm.NewEngine(inst.Config.DbDriver, inst.Config.DbDsn)
	if err != nil {
		log.Fatal(err)
	}
	defer inst.DB.Close()

	tables := []interface{}{new(LocalAccount), new(LocalTweet)}
	for _, table := range tables {
		err = inst.DB.Sync(table)
		if err != nil {
			log.Fatal(err)
		}
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	go handle_reload(&inst, c)

	ticker := time.NewTicker(time.Minute)
	go func() {
		for {
			<-ticker.C
			update_stats(inst)
		}
	}()
	update_stats(inst)

	r := mux.NewRouter()
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		funcMap := template.FuncMap{
			"fdate": func(t time.Time) string {
				return t.Format(time.RFC3339)
			},
		}
		path := "./main.html"
		index_tmpl := template.Must(template.New("").Funcs(funcMap).ParseFiles(path))
		err = index_tmpl.ExecuteTemplate(w, "main.html", inst)
		if err != nil {
			log.Fatal(err)
		}
	})
	r.HandleFunc("/.well-known/webfinger", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		resource := qs.Get("resource")
		if strings.HasPrefix(resource, "acct:") {
			full_name := resource[5:]
			parts := strings.Split(full_name, "@")
			if len(parts) != 2 {
				http.Error(w, "invalid acct: uri", http.StatusBadRequest)
				return
			}

			wf_obj := webfinger_acct(inst, parts[0])
			j, _ := json.Marshal(wf_obj)
			w.Header().Add("Content-Type", "application/jrd+json; charset=utf-8")
			w.Write(j)
		}
	})
	r.HandleFunc("/.well-known/host-meta", func(w http.ResponseWriter, r *http.Request) {
		host_meta := `<?xml version="1.0"?>
<XRD xmlns="http://docs.oasis-open.org/ns/xri/xrd-1.0">
  <Link rel="lrdd" type="application/xrd+xml" template="https://` + inst.Domain + `/.well-known/webfinger?resource={uri}"/>
</XRD>`

		w.Header().Add("Content-Type", "application/xrd+xml; charset=utf-8")
		fmt.Fprint(w, host_meta)
	})

	r.HandleFunc("/{name}/status/{id}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		id := vars["id"]

		url := fmt.Sprintf("https://%s/%s/status/%s", inst.Domain, name, id)
		link := fmt.Sprintf("<%s>; rel=\"alternate\"; type=\"application/activity+json\"", url)
		w.Header().Add("Link", link)

		if r.Method == "HEAD" {
			return
		}

		err := ap_check_headers(r)
		if err != nil {
			http.Redirect(w, r, fmt.Sprintf("https://twitter.com/%s/status/%s", name, id), 301)
			return
		}

		status, err := tw_get_status(inst, name, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var user LocalAccount
		user_hit, err := inst.DB.Sql("select * from local_account where local_id=?", status.LocalAccountId).Get(&user)
		if !user_hit {
			http.Error(w, "Account missing", http.StatusNotFound)
		}

		o := ap_note(inst, user, *status)
		o.Context = ap_context()
		j, _ := json.Marshal(o)
		w.Header().Add("Content-Type", "application/activity+json; charset=utf-8")
		w.Write(j)
	})
	r.HandleFunc("/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]

		url := fmt.Sprintf("https://%s/%s", inst.Domain, name)
		link := fmt.Sprintf("<%s>; rel=\"alternate\"; type=\"application/activity+json\"", url)
		w.Header().Add("Link", link)

		if r.Method == "HEAD" {
			return
		}

		err := ap_check_headers(r)
		if err != nil {
			http.Redirect(w, r, fmt.Sprintf("https://twitter.com/%s", name), 301)
			return
		}

		user, err := tw_get_user(inst, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		o := ap_person(inst, *user)
		o.Context = ap_context()

		j, _ := json.Marshal(o)
		w.Header().Add("Content-Type", "application/activity+json; charset=utf-8")
		w.Write(j)

	})
	r.HandleFunc("/{name}/outbox", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]

		err := ap_check_headers(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		user, err := tw_get_user(inst, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		outbox := ap_outbox(inst, *user)
		outbox.Context = ap_context()
		j, _ := json.Marshal(outbox)
		w.Header().Add("Content-Type", "application/activity+json; charset=utf-8")
		w.Write(j)
	})
	r.HandleFunc("/{name}/inbox", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if r.Body == nil {
				http.Error(w, "Please send a request body", http.StatusBadRequest)
				return
			}
			decoder := json.NewDecoder(r.Body)
			var t APActivity
			err := decoder.Decode(&t)
			if err != nil {
				http.Error(w, "json decode error: "+err.Error(), http.StatusBadRequest)
				return
			}
			// TODO: implement follow here
			return
		}

		http.Error(w, "", http.StatusNotFound)
		return
	})

	log.Printf("listening on: %s", inst.Config.Listen)
	http.ListenAndServe(inst.Config.Listen, Log(r))
}
