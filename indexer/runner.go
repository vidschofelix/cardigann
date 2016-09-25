package indexer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/Sirupsen/logrus"
	"github.com/cardigann/cardigann/config"
	"github.com/cardigann/cardigann/logger"
	"github.com/cardigann/cardigann/torznab"
	"github.com/dustin/go-humanize"
	"github.com/f2prateek/train"
	trainlog "github.com/f2prateek/train/log"
	"github.com/headzoo/surf"
	"github.com/headzoo/surf/agent"
	"github.com/headzoo/surf/browser"
	"github.com/headzoo/surf/jar"
	"github.com/yosssi/gohtml"
)

var (
	_ torznab.Indexer = &Runner{}
)

type RunnerOpts struct {
	Config     config.Config
	CachePages bool
}

type Runner struct {
	definition  *IndexerDefinition
	browser     browser.Browsable
	cookies     http.CookieJar
	opts        RunnerOpts
	logger      logrus.FieldLogger
	caps        torznab.Capabilities
	browserLock sync.Mutex
}

func NewRunner(def *IndexerDefinition, opts RunnerOpts) *Runner {
	return &Runner{
		opts:       opts,
		definition: def,
		logger:     logger.Logger.WithFields(logrus.Fields{"site": def.Site}),
	}
}

func (r *Runner) createBrowser() {
	r.browserLock.Lock()

	if r.cookies == nil {
		r.cookies = jar.NewMemoryCookies()
	}

	bow := surf.NewBrowser()
	bow.SetUserAgent(agent.Chrome())
	bow.SetAttribute(browser.SendReferer, false)
	bow.SetAttribute(browser.MetaRefreshHandling, true)
	bow.SetCookieJar(r.cookies)

	switch os.Getenv("DEBUG_HTTP") {
	case "1", "true", "basic":
		bow.SetTransport(train.Transport(trainlog.New(os.Stderr, trainlog.Basic)))
	case "body":
		bow.SetTransport(train.Transport(trainlog.New(os.Stderr, trainlog.Body)))
	}

	r.browser = bow
}

func (r *Runner) releaseBrowser() {
	r.browser = nil
	r.browserLock.Unlock()
}

func (r *Runner) applyTemplate(name, tpl string, ctx interface{}) (string, error) {
	tmpl, err := template.New(name).Parse(tpl)
	if err != nil {
		return "", err
	}
	b := &bytes.Buffer{}
	err = tmpl.Execute(b, ctx)
	if err != nil {
		return "", err
	}
	if strings.Contains(tpl, "{{") {
		r.logger.
			WithFields(logrus.Fields{"src": tpl, "result": b.String(), "ctx": ctx}).
			Debugf("Processed template")
	}
	return b.String(), nil
}

func (r *Runner) currentURL() (*url.URL, error) {
	if u := r.browser.Url(); u != nil {
		return u, nil
	}

	configURL, ok, _ := r.opts.Config.Get(r.definition.Site, "url")
	if ok && r.testURLWorks(configURL) {
		return url.Parse(configURL)
	}

	for _, u := range r.definition.Links {
		if u != configURL && r.testURLWorks(u) {
			return url.Parse(u)
		}
	}

	return nil, errors.New("No working urls found")
}

func (r *Runner) testURLWorks(u string) bool {
	r.logger.WithField("url", u).Debugf("Checking connectivity to url")

	err := r.browser.Open(u)
	if err != nil {
		r.logger.WithError(err).Warn("URL check failed")
		return false
	} else if r.browser.StatusCode() != http.StatusOK {
		r.logger.Warn("URL returned non-ok status")
		return false
	}

	return true
}

func (r *Runner) resolvePath(urlPath string) (string, error) {
	base, err := r.currentURL()
	if err != nil {
		return "", err
	}

	u, err := url.Parse(urlPath)
	if err != nil {
		return "", err
	}

	resolved := base.ResolveReference(u)
	r.logger.
		WithFields(logrus.Fields{"base": base.String(), "u": resolved.String()}).
		Debugf("Resolving url")

	return resolved.String(), nil
}

// this should eventually upstream into surf browser
func (r *Runner) handleMetaRefreshHeader() error {
	h := r.browser.ResponseHeaders()

	if refresh := h.Get("Refresh"); refresh != "" {
		if s := regexp.MustCompile(`\s*;\s*`).Split(refresh, 2); len(s) == 2 {
			r.logger.
				WithField("fields", s).
				Debug("Found refresh header")

			u, err := r.resolvePath(strings.TrimPrefix(s[1], "url="))
			if err != nil {
				return err
			}

			return r.openPage(u)
		}
	}
	return nil
}

func (r *Runner) openPage(u string) error {
	r.logger.WithField("url", u).Debug("Opening page")

	err := r.browser.Open(u)
	if err != nil {
		return err
	}

	r.cachePage()

	r.logger.
		WithFields(logrus.Fields{"code": r.browser.StatusCode(), "page": r.browser.Url()}).
		Debugf("Finished request")

	if err = r.handleMetaRefreshHeader(); err != nil {
		return err
	}

	return nil
}

func (r *Runner) postToPage(u string, vals url.Values) error {
	r.logger.
		WithFields(logrus.Fields{"url": u, "vals": vals}).
		Debugf("Posting to page")

	if err := r.browser.PostForm(u, vals); err != nil {
		return err
	}

	r.cachePage()

	r.logger.
		WithFields(logrus.Fields{"code": r.browser.StatusCode(), "page": r.browser.Url()}).
		Debugf("Finished request")

	if err := r.handleMetaRefreshHeader(); err != nil {
		return err
	}

	return nil
}

func (r *Runner) cachePage() error {
	if !r.opts.CachePages {
		return nil
	}

	dir := config.GetCachePath(r.definition.Site)

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	tmpfile, err := ioutil.TempFile(dir, "pagecache")
	if err != nil {
		r.logger.Warn(err)
		return err
	}

	body := strings.NewReader(r.browser.Body())
	io.Copy(tmpfile, body)
	defer tmpfile.Close()

	r.logger.
		WithFields(logrus.Fields{"file": "file://" + tmpfile.Name()}).
		Debugf("Wrote page output to cache")

	return nil
}

func (r *Runner) loginViaForm(loginURL, formSelector string, vals map[string]string) error {
	r.logger.
		WithFields(logrus.Fields{"url": loginURL, "form": formSelector, "vals": vals}).
		Debugf("Filling and submitting login form")

	if err := r.openPage(loginURL); err != nil {
		return err
	}

	fm, err := r.browser.Form(formSelector)
	if err != nil {
		return err
	}

	for name, value := range vals {
		r.logger.
			WithFields(logrus.Fields{"key": name, "form": formSelector, "val": value}).
			Debugf("Filling input of form")

		if err = fm.Input(name, value); err != nil {
			r.logger.WithError(err).Error("Filling input failed")
			return err
		}
	}

	r.logger.Debug("Submitting login form")
	defer r.cachePage()

	if err = fm.Submit(); err != nil {
		r.logger.WithError(err).Error("Login failed")
		return err
	}

	r.logger.
		WithFields(logrus.Fields{"code": r.browser.StatusCode(), "page": r.browser.Url()}).
		Debugf("Submitted login form")

	return nil
}

func (r *Runner) loginViaPost(loginURL string, vals map[string]string) error {
	data := url.Values{}
	for key, value := range vals {
		data.Add(key, value)
	}

	return r.postToPage(loginURL, data)
}

func parseCookieString(cookie string) []*http.Cookie {
	h := http.Header{"Cookie": []string{cookie}}
	r := http.Request{Header: h}
	return r.Cookies()
}

func (r *Runner) loginViaCookie(loginURL string, cookie string) error {
	u, err := url.Parse(loginURL)
	if err != nil {
		return err
	}

	cookies := parseCookieString(cookie)

	r.logger.
		WithFields(logrus.Fields{"url": loginURL, "cookies": cookies}).
		Debugf("Setting cookies for login")

	cj := jar.NewMemoryCookies()
	cj.SetCookies(u, cookies)

	r.browser.SetCookieJar(cj)
	return nil
}

func (r *Runner) extractInputLogins() (map[string]string, error) {
	result := map[string]string{}

	cfg, err := r.opts.Config.Section(r.definition.Site)
	if err != nil {
		return nil, err
	}

	ctx := struct {
		Config map[string]string
	}{
		cfg,
	}

	for name, val := range r.definition.Login.Inputs {
		resolved, err := r.applyTemplate("login_inputs", val, ctx)
		if err != nil {
			return nil, err
		}

		r.logger.
			WithFields(logrus.Fields{"key": name, "val": resolved}).
			Debugf("Resolved login input template")

		result[name] = resolved
	}

	return result, nil
}

func (r *Runner) isLoginRequired() (bool, error) {
	if r.definition.Login.Path == "" && r.definition.Login.Method == "" {
		return false, nil
	} else if r.definition.Login.Test.Path == "" {
		return true, nil
	}

	r.logger.
		WithField("path", r.definition.Login.Test).
		Debug("Testing if login is needed")

	testUrl, err := r.resolvePath(r.definition.Login.Test.Path)
	if err != nil {
		return true, err
	}

	err = r.openPage(testUrl)
	if err != nil {
		r.logger.WithError(err).Warn("Failed to open page")
		return true, nil
	}

	if testUrl == r.browser.Url().String() {
		r.logger.Debug("No login needed, already logged in")
		return false, nil
	}

	r.logger.Debug("Login is required")
	return true, nil
}

func (r *Runner) login() error {
	if r.browser == nil {
		r.createBrowser()
		defer r.releaseBrowser()
	}

	filterLogger = r.logger

	loginUrl, err := r.resolvePath(r.definition.Login.Path)
	if err != nil {
		return err
	}

	vals, err := r.extractInputLogins()
	if err != nil {
		return err
	}

	switch r.definition.Login.Method {
	case "", loginMethodForm:
		if err = r.loginViaForm(loginUrl, r.definition.Login.FormSelector, vals); err != nil {
			return err
		}
	case loginMethodPost:
		if err = r.loginViaPost(loginUrl, vals); err != nil {
			return err
		}
	case loginMethodCookie:
		if err = r.loginViaCookie(loginUrl, vals["cookie"]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Unknown login method %q", r.definition.Login.Method)
	}

	if len(r.definition.Login.Error) > 0 {
		r.logger.
			WithField("block", r.definition.Login.Error).
			Debug("Testing if login succeeded")

		if err = r.definition.Login.hasError(r.browser); err != nil {
			r.logger.WithError(err).Error("Failed to login")
			return err
		}
	}

	if r.definition.Login.Test.Path != "" {
		if required, err := r.isLoginRequired(); err != nil {
			return err
		} else if required {
			return errors.New("Login check after login failed")
		}
	}

	r.logger.Info("Successfully logged in")
	return nil
}

func (r *Runner) Info() torznab.Info {
	return torznab.Info{
		ID:       r.definition.Site,
		Title:    r.definition.Name,
		Language: r.definition.Language,
		Link:     r.definition.Links[0],
	}
}

func (r *Runner) Capabilities() torznab.Capabilities {
	return r.definition.Capabilities.ToTorznab()
}

type extractedItem struct {
	torznab.ResultItem
	LocalCategoryID string
}

// localCategories returns a slice of local categories that should be searched
func (r *Runner) localCategories(query torznab.Query) []string {
	localCats := []string{}

	if len(query.Categories) > 0 {
		queryCats := torznab.AllCategories.Subset(query.Categories...)

		// resolve query categories to the exact local, or the local based on parent cat
		for _, id := range r.definition.Capabilities.CategoryMap.ResolveAll(queryCats...) {
			localCats = append(localCats, id)
		}

		r.logger.
			WithFields(logrus.Fields{"querycats": query.Categories, "local": localCats}).
			Debugf("Resolved torznab cats to local")
	}

	return localCats
}

func (r *Runner) Search(query torznab.Query) ([]torznab.ResultItem, error) {
	r.createBrowser()
	defer r.releaseBrowser()

	// TODO: make this concurrency safe
	filterLogger = r.logger

	if required, err := r.isLoginRequired(); err != nil {
		return nil, err
	} else if required {
		if err := r.login(); err != nil {
			r.logger.WithError(err).Error("Login failed")
			return nil, err
		}
	}

	localCats := r.localCategories(query)

	templateCtx := struct {
		Query      torznab.Query
		Keywords   string
		Categories []string
	}{
		query,
		query.Keywords(),
		localCats,
	}

	searchURL, err := r.applyTemplate("search_path", r.definition.Search.Path, templateCtx)
	if err != nil {
		return nil, err
	}

	searchURL, err = r.resolvePath(searchURL)
	if err != nil {
		return nil, err
	}

	r.logger.
		WithFields(logrus.Fields{"query": query.Encode()}).
		Infof("Searching indexer")

	vals := url.Values{}

	for name, val := range r.definition.Search.Inputs {
		resolved, err := r.applyTemplate("search_inputs", val, templateCtx)
		if err != nil {
			return nil, err
		}
		switch name {
		case "$raw":
			parsedVals, err := url.ParseQuery(resolved)
			if err != nil {
				r.logger.WithError(err).Warn(err)
				return nil, fmt.Errorf("Error parsing $raw input: %s", err.Error())
			}

			r.logger.
				WithFields(logrus.Fields{"source": val, "parsed": parsedVals}).
				Debugf("Processed $raw input")

			for k, values := range parsedVals {
				for _, val := range values {
					vals.Add(k, val)
				}
			}
		default:
			vals.Add(name, resolved)
		}
	}

	timer := time.Now()

	switch r.definition.Search.Method {
	case "", searchMethodGet:
		if len(vals) > 0 {
			searchURL = fmt.Sprintf("%s?%s", searchURL, vals.Encode())
		}
		if err = r.openPage(searchURL); err != nil {
			return nil, err
		}
	case searchMethodPost:
		if err = r.postToPage(searchURL, vals); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("Unknown search method %q", r.definition.Search.Method)
	}

	dom := r.browser.Dom()

	// merge following rows for After selector
	if after := r.definition.Search.Rows.After; after > 0 {
		rows := dom.Find(r.definition.Search.Rows.Selector)
		for i := 0; i < rows.Length(); i += 1 + after {
			rows.Eq(i).AppendSelection(rows.Slice(i+1, i+1+after).Find("td"))
			rows.Slice(i+1, i+1+after).Remove()
		}
	}

	// apply Remove if it exists
	if remove := r.definition.Search.Rows.Remove; remove != "" {
		matching := dom.Find(r.definition.Search.Rows.Selector).Filter(remove)
		r.logger.
			WithFields(logrus.Fields{"selector": remove}).
			Debugf("Applying remove to %d rows", matching.Length())
		matching.Remove()
	}

	rows := dom.Find(r.definition.Search.Rows.Selector)

	r.logger.
		WithFields(logrus.Fields{
			"rows":     rows.Length(),
			"selector": r.definition.Search.Rows.Selector,
			"limit":    query.Limit,
			"offset":   query.Offset,
		}).Debugf("Found %d rows", rows.Length())

	extracted := []extractedItem{}

	for i := 0; i < rows.Length(); i++ {
		if query.Limit > 0 && len(extracted) >= query.Limit {
			break
		}

		item, err := r.extractItem(i+1, rows.Eq(i))
		if err != nil {
			return nil, err
		}

		var matchCat bool
		if len(localCats) > 0 {
			for _, catId := range localCats {
				if catId == item.LocalCategoryID {
					matchCat = true
				}
			}

			if !matchCat {
				r.logger.
					WithFields(logrus.Fields{"id": item.LocalCategoryID, "localCats": localCats}).
					Debug("Skipping non-matching category")
				continue
			}
		}

		if mappedCat, ok := r.definition.Capabilities.CategoryMap[item.LocalCategoryID]; ok {
			item.Category = mappedCat.ID
		} else {
			r.logger.
				WithFields(logrus.Fields{"localId": item.LocalCategoryID}).
				Warn("Unknown local category")

			if intCatId, err := strconv.Atoi(item.LocalCategoryID); err == nil {
				item.Category = intCatId + torznab.CustomCategoryOffset
			}
		}

		extracted = append(extracted, item)
	}

	r.logger.
		WithFields(logrus.Fields{"time": time.Now().Sub(timer)}).
		Infof("Query returned %d results", len(extracted))

	items := []torznab.ResultItem{}
	for _, item := range extracted {
		items = append(items, item.ResultItem)
	}

	return items, nil
}

func (r *Runner) extractItem(rowIdx int, selection *goquery.Selection) (extractedItem, error) {
	row := map[string]string{}

	html, _ := goquery.OuterHtml(selection)
	r.logger.WithFields(logrus.Fields{"html": gohtml.Format(html)}).Debug("Processing row")

	for _, item := range r.definition.Search.Fields {
		r.logger.
			WithFields(logrus.Fields{"row": rowIdx, "block": item.Block.String()}).
			Debugf("Processing field %q", item.Field)

		val, err := item.Block.MatchText(selection)
		if err != nil {
			return extractedItem{}, err
		}

		r.logger.
			WithFields(logrus.Fields{"row": rowIdx, "output": val}).
			Debugf("Finished processing field %q", item.Field)

		row[item.Field] = val
	}

	item := extractedItem{
		ResultItem: torznab.ResultItem{
			Site:            r.definition.Site,
			MinimumRatio:    1,
			MinimumSeedTime: time.Hour * 48,
		},
	}

	r.logger.
		WithFields(logrus.Fields{"row": rowIdx, "data": row}).
		Debugf("Finished row %d", rowIdx)

	for key, val := range row {
		switch key {
		case "download":
			u, err := r.resolvePath(val)
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable url %q in %s", rowIdx, val, key)
				continue
			}
			item.Link = u
		case "details":
			u, err := r.resolvePath(val)
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable url %q in %s", rowIdx, val, key)
				continue
			}
			item.GUID = u
		case "comments":
			u, err := r.resolvePath(val)
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable url %q in %s", rowIdx, val, key)
				continue
			}
			item.Comments = u
		case "title":
			item.Title = val
		case "description":
			item.Description = val
		case "category":
			item.LocalCategoryID = val
		case "size":
			bytes, err := humanize.ParseBytes(strings.Replace(val, ",", "", -1))
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable size %q: %v", rowIdx, val, err.Error())
				continue
			}
			r.logger.Debugf("After parsing, size is %v", bytes)
			item.Size = bytes
		case "leechers":
			leechers, err := strconv.Atoi(val)
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable leechers value %q in %s", rowIdx, val, key)
				continue
			}
			item.Peers += leechers
		case "seeders":
			seeders, err := strconv.Atoi(val)
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable seeders value %q in %s", rowIdx, val, key)
				continue
			}
			item.Seeders = seeders
			item.Peers += seeders
		case "date":
			t, err := parseFuzzyTime(val, time.Now())
			if err != nil {
				r.logger.Warnf("Row #%d has unparseable time %q in %s", rowIdx, val, key)
				continue
			}
			item.PublishDate = t
		default:
			r.logger.Warnf("Row #%d has unknown field %s", rowIdx, key)
			continue
		}
	}

	if item.GUID == "" && item.Link != "" {
		item.GUID = item.Link
	}

	if r.hasDateHeader() {
		date, err := r.extractDateHeader(selection)
		if err != nil {
			return extractedItem{}, err
		}

		item.PublishDate = date
	}

	return item, nil
}

func (r *Runner) hasDateHeader() bool {
	return !r.definition.Search.Rows.DateHeaders.IsEmpty()
}

func (r *Runner) extractDateHeader(selection *goquery.Selection) (time.Time, error) {
	dateHeaders := r.definition.Search.Rows.DateHeaders

	r.logger.
		WithFields(logrus.Fields{"selector": dateHeaders.String()}).
		Debugf("Searching for date header")

	prev := selection.PrevAllFiltered(dateHeaders.Selector).First()
	if prev.Length() == 0 {
		return time.Time{}, fmt.Errorf("No date header row found")
	}

	dv, _ := dateHeaders.Text(prev.First())
	return parseFuzzyTime(dv, time.Now())
}

func (r *Runner) Download(u string) (io.ReadCloser, http.Header, error) {
	r.createBrowser()

	if required, err := r.isLoginRequired(); required {
		if err := r.login(); err != nil {
			r.logger.WithError(err).Error("Login failed")
			return nil, http.Header{}, err
		}
	} else if err != nil {
		return nil, http.Header{}, err
	}

	fullUrl, err := r.resolvePath(u)
	if err != nil {
		return nil, http.Header{}, err
	}

	if err := r.browser.Open(fullUrl); err != nil {
		return nil, http.Header{}, err
	}

	pipeR, pipeW := io.Pipe()
	go func() {
		defer pipeW.Close()
		defer r.releaseBrowser()
		n, err := r.browser.Download(pipeW)
		if err != nil {
			r.logger.Error(err)
		}
		r.logger.WithFields(logrus.Fields{"url": fullUrl}).Debugf("Downloaded %d bytes", n)
	}()

	return pipeR, r.browser.ResponseHeaders(), nil
}
