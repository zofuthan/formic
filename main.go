package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/antonholmquist/jason"
	"github.com/drone/config"
	"github.com/dustin/randbo"
	"github.com/garyburd/redigo/redis"
	"github.com/unrolled/render"
	"github.com/zenazn/goji"
	"github.com/zenazn/goji/web"
	"github.com/zenazn/goji/web/middleware"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"gopkg.in/boj/redistore.v1"
)

type Form struct {
	ID          string
	Name        string
	RedirectURL string
}

var (
	r                   *render.Render
	rp                  *redis.Pool
	rs                  *redistore.RediStore
	sessionSecret       = config.String("session-secret", "")
	googleClientID      = config.String("google-client-id", "")
	googleClientSecret  = config.String("google-client-secret", "")
	googleAllowedEmails = config.String("google-allowed-emails", "")
)

func key(args ...string) string {
	args = append([]string{"submit"}, args...)
	return strings.Join(args, ":")
}

func genID() string {
	p := make([]byte, 8)
	randbo.New().Read(p)
	return fmt.Sprintf("%x", p)
}

func getForm(rc redis.Conn, id string, form *Form) error {
	v, err := redis.Values(
		rc.Do("HGETALL", key("form", id)),
	)
	if err != nil {
		return err
	}
	redis.ScanStruct(v, form)
	return nil
}

func createURL(req *http.Request) url.URL {
	var url_ *url.URL
	url_ = req.URL
	url_.Scheme = "http"
	if req.TLS != nil {
		url_.Scheme += "s"
	}
	url_.Host = req.Host
	url_.RawQuery = ""
	url_.Fragment = ""
	return *url_
}

func loginGoogleConfig(req *http.Request) *oauth2.Config {
	redirectURL := createURL(req)
	redirectURL.Path = "/oauth2callback"
	fmt.Println(redirectURL.String())
	return &oauth2.Config{
		ClientID:     *googleClientID,
		ClientSecret: *googleClientSecret,
		RedirectURL:  redirectURL.String(),
		Scopes:       []string{"email"},
		Endpoint:     google.Endpoint,
	}
}

func requireLogin(c *web.C, h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, req *http.Request) {
		session, err := rs.Get(req, "session")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, loggedIn := session.Values["loggedIn"]

		if !loggedIn {
			gc := loginGoogleConfig(req)
			authURL := gc.AuthCodeURL("")
			http.Redirect(w, req, authURL, http.StatusFound)
			return
		}

		h.ServeHTTP(w, req)
	}
	return http.HandlerFunc(fn)
}

func index(c web.C, w http.ResponseWriter, req *http.Request) {
	r.HTML(w, http.StatusOK, "index", nil)
}

func login(c web.C, w http.ResponseWriter, req *http.Request) {
	var err error

	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}()

	code := req.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	gc := loginGoogleConfig(req)
	tok, err := gc.Exchange(oauth2.NoContext, code)
	if err != nil {
		return
	}

	cli := gc.Client(oauth2.NoContext, tok)
	resp, err := cli.Get("https://www.googleapis.com/plus/v1/people/me")
	if err != nil {
		return
	}

	data, err := jason.NewObjectFromReader(resp.Body)
	if err != nil {
		return
	}

	emails, err := data.GetObjectArray("emails")
	if err != nil {
		return
	}

	var email string
	for _, e := range emails {
		email, err = e.GetString("value")
		if err != nil {
			return
		}
		break
	}

	allowedEmails := strings.Split(*googleAllowedEmails, ",")
	for _, allowedEmail := range allowedEmails {
		if email == allowedEmail {
			session, err := rs.Get(req, "session")
			if err != nil {
				return
			}

			session.Values["loggedIn"] = true
			err = session.Save(req, w)
			if err != nil {
				return
			}

			http.Redirect(w, req, "/admin/", http.StatusFound)
		}
	}

	http.Redirect(w, req, "/", http.StatusFound)
}

func showForms(c web.C, w http.ResponseWriter, req *http.Request) {
	var (
		forms []Form
		err   error
	)
	rc := rp.Get()

	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}()

	fids, err := redis.Strings(rc.Do(
		"SMEMBERS",
		key("forms"),
	))
	if err != nil {
		return
	}

	for _, fid := range fids {
		var form Form
		err = getForm(rc, fid, &form)
		if err != nil {
			return
		}
		forms = append(forms, form)
	}

	r.HTML(w, http.StatusOK, "forms", map[string]interface{}{
		"Forms": forms,
	})
}

func createForm(c web.C, w http.ResponseWriter, req *http.Request) {
	var (
		formName    string
		redirectURL string
		err         error
	)
	rc := rp.Get()

	defer func() {
		if err == nil {
			id := genID()

			rc.Do("HMSET", key("form", id),
				"ID", id,
				"Name", formName,
				"RedirectURL", redirectURL,
			)

			rc.Do("SADD", key("forms"), id)

			url := fmt.Sprintf("/admin/%s", id)
			http.Redirect(w, req, url, http.StatusFound)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}()

	if err = req.ParseForm(); err != nil {
		return
	}

	formName = req.PostForm.Get("formName")
	if formName == "" {
		err = errors.New("Form name can't be empty")
		return
	}

	redirectURL = req.PostForm.Get("redirectURL")
	if redirectURL == "" {
		err = errors.New("Redirect URL can't be empty")
		return
	}
}

func logout(c web.C, w http.ResponseWriter, req *http.Request) {
	var err error

	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}()

	session, err := rs.Get(req, "session")
	if err != nil {
		return
	}

	session.Options.MaxAge = -1
	err = session.Save(req, w)
	if err != nil {
		return
	}

	http.Redirect(w, req, "/", http.StatusFound)
}

func showForm(c web.C, w http.ResponseWriter, req *http.Request) {
	var (
		form    Form
		entries []interface{}
		err     error
	)
	rc := rp.Get()

	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}()

	err = getForm(rc, c.URLParams["id"], &form)
	if err != nil {
		return
	}

	if form == (Form{}) {
		http.Error(w, "Form doesn't exist", http.StatusNotFound)
		return
	}

	formURL := createURL(req)
	formURL.Path = fmt.Sprintf("/s/%s", form.ID)

	fields, err := redis.Strings(rc.Do(
		"SMEMBERS",
		key("form", form.ID, "fields"),
	))
	if err != nil {
		return
	}

	eids, err := redis.Strings(rc.Do(
		"SMEMBERS",
		key("form", form.ID, "entries"),
	))

	if err != nil {
		return
	}

	for _, eid := range eids {
		v, err := redis.Strings(
			rc.Do("HGETALL", key("form", form.ID, "entry", eid)),
		)

		if err != nil {
			return
		}

		entry := make(map[string]string)
		for i := 0; i < len(v); i += 2 {
			entry[v[i]] = v[i+1]
		}

		entries = append(entries, entry)
	}

	r.HTML(w, http.StatusOK, "form", map[string]interface{}{
		"Name":    form.Name,
		"URL":     formURL.String(),
		"Fields":  fields,
		"Entries": entries,
	})
}

func submitEntry(c web.C, w http.ResponseWriter, req *http.Request) {
	var (
		form Form
		err  error
	)
	rc := rp.Get()

	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}()

	err = getForm(rc, c.URLParams["id"], &form)
	if err != nil {
		return
	}

	if form == (Form{}) {
		http.Error(w, "Form doesn't exist", http.StatusNotFound)
		return
	}

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	eid := genID()

	entry := []interface{}{key("form", form.ID, "entry", eid)}
	for field := range req.PostForm {
		entry = append(entry, field, req.PostForm.Get(field))
		rc.Do("SADD", key("form", form.ID, "fields"), field)
	}
	rc.Do("HMSET", entry...)

	rc.Do("SADD", key("form", form.ID, "entries"), eid)

	http.Redirect(w, req, form.RedirectURL, http.StatusFound)
}

func init() {
	r = render.New(render.Options{
		Layout:        "layout",
		IsDevelopment: true,
	})
	rh := os.Getenv("REDIS_HOST")
	if rh == "" {
		rh = "localhost"
	}
	rp = &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", fmt.Sprintf(
				"%s:6379",
				rh,
			))
			if err != nil {
				return nil, err
			}
			return c, err
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
	rs, _ = redistore.NewRediStoreWithPool(rp, []byte(*sessionSecret))
}

func main() {
	config.SetPrefix("SUBMIT_")
	config.Parse("submit.conf")

	goji.Get("/", index)
	goji.Get("/oauth2callback", login)
	goji.Get("/logout", logout)

	admin := web.New()
	admin.Use(middleware.SubRouter)
	admin.Use(requireLogin)
	admin.Get("/", showForms)
	admin.Post("/", createForm)
	admin.Get("/:id", showForm)
	goji.Handle("/admin/*", admin)

	goji.Post("/s/:id", submitEntry)

	goji.Get("/static/lib/*", http.StripPrefix(
		"/static/lib/",
		http.FileServer(
			http.Dir("./bower_components"),
		),
	))
	goji.Get("/static/*", http.StripPrefix(
		"/static/",
		http.FileServer(
			http.Dir("./static"),
		),
	))

	goji.Serve()
}