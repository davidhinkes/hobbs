package nms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
)

const (
	testID     = "test-client-id-0000"
	testSecret = "test-client-secret-SHOULD-NEVER-LEAK"
)

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func sample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "nms", "notams_sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fake is a stand-in NMS server.
type fake struct {
	srv         *httptest.Server
	tokenCalls  atomic.Int32
	apiCalls    atomic.Int32
	expiresIn   string // token lifetime reported
	tokenStatus int
	apiStatus   int
	apiBody     []byte
	// rejectFirstToken makes the API answer 401 to the first token it issued.
	rejectFirstToken bool
	lastReq          atomic.Value // *http.Request (headers/query only)
	issued           atomic.Int32
}

func newFake(t *testing.T, body []byte) (*fake, *Client) {
	t.Helper()
	f := &fake{expiresIn: "1799", tokenStatus: 200, apiStatus: 200, apiBody: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		u, p, ok := r.BasicAuth()
		if r.Method != http.MethodPost || !ok || u != testID || p != testSecret || r.FormValue("grant_type") != "client_credentials" {
			w.WriteHeader(401)
			w.Write([]byte(`{"ErrorCode":"invalid_client","Error":"Client credentials are invalid"}`))
			return
		}
		if f.tokenStatus != 200 {
			w.WriteHeader(f.tokenStatus)
			return
		}
		n := f.issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-` + string(rune('0'+n)) + `","expires_in":"` + f.expiresIn + `","token_type":"BearerToken","status":"approved"}`))
	})
	mux.HandleFunc("/nmsapi/v1/notams", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		f.lastReq.Store(r.Clone(context.Background()))
		auth := r.Header.Get("Authorization")
		if auth == "" || (f.rejectFirstToken && auth == "Bearer tok-1") {
			w.WriteHeader(401)
			w.Write([]byte(`{"timestamp":"x","status":"401","message":"Access Token is Invalid or Expired"}`))
			return
		}
		if r.Header.Get("nmsResponseFormat") == "" {
			w.WriteHeader(400)
			w.Write([]byte(`{"status":"Failure","message":"BAD_REQUEST: Required request header 'nmsResponseFormat' is not present"}`))
			return
		}
		w.WriteHeader(f.apiStatus)
		w.Write(f.apiBody)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	c, err := NewClient(Config{Host: f.srv.URL, ClientID: testID, ClientSecret: testSecret, Production: true, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

var q = Query{Location: "KIAD"}

func TestQueryParsesSampleAndSendsRequiredHeaders(t *testing.T) {
	f, c := newFake(t, sample(t))
	snap, err := c.Query(context.Background(), Query{Location: "KIAD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 16 || snap.Source.RecordsInFeed != 16 {
		t.Fatalf("items=%d feed=%d", len(snap.Items), snap.Source.RecordsInFeed)
	}
	r := f.lastReq.Load().(*http.Request)
	if r.Header.Get("nmsResponseFormat") != "GEOJSON" || r.Header.Get("Authorization") != "Bearer tok-1" {
		t.Errorf("headers = %v", r.Header)
	}
	if r.URL.Query().Get("location") != "KIAD" {
		t.Errorf("query = %v", r.URL.RawQuery)
	}
	s := snap.Source
	if s.ID != SourceID || s.Authority != Authority || s.Documentation != provenance.Documented || s.HTTPStatus != 200 ||
		s.ContentSHA256 == "" || !s.RetrievedAt.Equal(fixedNow) || s.UpstreamTimestamp == nil || s.RecordsReturned != 16 {
		t.Errorf("source = %+v", s)
	}
	for _, a := range snap.Items {
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
		if a.RawText == "" {
			t.Errorf("%s: raw text lost", a.ID)
		}
	}
	if _, err := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues); err != nil {
		t.Errorf("envelope: %v", err)
	}
}

func byNumber(t *testing.T, items []model.Notam, num string) model.Notam {
	t.Helper()
	for _, a := range items {
		if a.ID == num {
			return a
		}
	}
	t.Fatalf("no NOTAM %q", num)
	return model.Notam{}
}

func TestSampleRecordSemantics(t *testing.T) {
	_, c := newFake(t, sample(t))
	snap, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	it := snap.Items

	perm := byNumber(t, it, "04/273")
	if !perm.Permanent || perm.EffectiveTo != nil || perm.EffectiveFrom == nil {
		t.Errorf("PERM: %+v", perm)
	}

	if cancel := byNumber(t, it, "A6032/26"); cancel.Type != "C" {
		t.Errorf("type = %q", cancel.Type)
	}
	if notype := byNumber(t, it, "4/9383"); notype.Type != "" || len(notype.ParseIssues) != 0 {
		t.Errorf("a missing type is left empty, not guessed or flagged: %q %v", notype.Type, notype.ParseIssues)
	}

	// Limits: explicit F/G limits win; datum preserved.
	agl := byNumber(t, it, "A2261/26")
	if agl.Lower.Datum != model.DatumAGL || *agl.Lower.Value != 100 || agl.Upper.Datum != model.DatumMSL || *agl.Upper.Value != 3000 {
		t.Errorf("AGL/AMSL limits: lower=%+v upper=%+v", agl.Lower, agl.Upper)
	}
	fl := byNumber(t, it, "11/721")
	if fl.Lower.Datum != model.DatumSFC || fl.Upper.Datum != model.DatumFL || *fl.Upper.Value != 230 {
		t.Errorf("SFC-FL230: lower=%+v upper=%+v", fl.Lower, fl.Upper)
	}
	if unl := byNumber(t, it, "11/773"); unl.Upper.Datum != model.DatumUnl {
		t.Errorf("UNL upper = %+v", unl.Upper)
	}
	// "135FT" has no datum: not guessed; falls back to the Q-line level and is flagged.
	bare := byNumber(t, it, "A2141/26")
	if len(bare.ParseIssues) == 0 || !strings.Contains(strings.Join(bare.ParseIssues, " "), "no usable datum") {
		t.Errorf("bare feet must be flagged: %v", bare.ParseIssues)
	}
	if bare.Upper.Datum != model.DatumFL {
		t.Errorf("bare feet should fall back to the Q-line FL, got %+v", bare.Upper)
	}
	// No limits at all: unknown, never zero.
	if none := byNumber(t, it, "09/105"); none.Lower.Known() || none.Upper.Known() {
		t.Errorf("no stated limits must stay unknown: %+v %+v", none.Lower, none.Upper)
	}

	// Schedules are passed through as published and flagged; the server does not interpret them.
	sch := byNumber(t, it, "10/640")
	if sch.Schedule == "" {
		t.Fatal("schedule lost")
	}
	if sch.EffectiveFrom == nil || sch.EffectiveTo == nil {
		t.Errorf("a scheduled NOTAM keeps its effective period as published: %+v", sch)
	}
	if issue(snap.Issues, provenance.ScheduleUnevaluable) == nil {
		t.Error("expected SCHEDULE_UNEVALUABLE")
	}

	// Estimated flag.
	if est := byNumber(t, it, "6/6180"); !est.Estimated {
		t.Error("estimated flag lost")
	}

	// Geometry shapes.
	if g := byNumber(t, it, "04/035").Geometry; g == nil || g.Center == nil || g.RadiusNM == nil {
		t.Errorf("point+radius should be a circle: %+v", g)
	}
	if g := byNumber(t, it, "10/041").Geometry; g == nil || len(g.Ring) < 4 {
		t.Errorf("polygon: %+v", g)
	}
	if g := byNumber(t, it, "11/151").Geometry; g == nil || len(g.Rings) != 3 {
		t.Errorf("multipolygon should keep all 3 parts: %+v", g)
	}

	// Text: ICAO preferred; every rendering kept; single-translation records kept.
	for _, a := range it {
		if len(a.Translations) == 0 {
			t.Errorf("%s: no translations", a.ID)
		}
	}
	icaoOnly := byNumber(t, it, "A5801/26")
	if !strings.Contains(icaoOnly.RawText, "NOTAM") || len(icaoOnly.Translations) != 1 {
		t.Errorf("icao-only: %q %v", icaoOnly.RawText, icaoOnly.Translations)
	}
	localOnly := byNumber(t, it, "09/004")
	if !strings.HasPrefix(localOnly.RawText, "!") || len(localOnly.Translations) != 1 {
		t.Errorf("local-only: %q", localOnly.RawText)
	}
	for _, a := range it {
		if len(a.Translations) == 2 && a.RawText != a.Translations[0].Text && a.RawText != a.Translations[1].Text {
			t.Errorf("%s: RawText must be one of the renderings, unaltered", a.ID)
		}
		if a.NMSID == "" || a.Classification == "" {
			t.Errorf("%s: missing nms id/classification", a.ID)
		}
	}
	if mil := byNumber(t, it, "M0832/26"); mil.Classification != "MIL" {
		t.Errorf("classification = %q", mil.Classification)
	}
}

func issue(is []provenance.Issue, c provenance.Code) *provenance.Issue {
	for i := range is {
		if is[i].Code == c {
			return &is[i]
		}
	}
	return nil
}

func TestEmptyDataObjectIsAGenuineNone(t *testing.T) {
	_, c := newFake(t, []byte(`{"status":"Success","data":{}}`))
	snap, err := c.Query(context.Background(), q)
	if err != nil || len(snap.Items) != 0 {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	// Production, clean pull: the envelope may assert "none".
	env, err := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if err != nil || !env.Completeness.CanAssertNone() {
		t.Errorf("env=%+v err=%v", env.Completeness, err)
	}
}

func TestStagingIsFlaggedAndCannotAssertNone(t *testing.T) {
	f, _ := newFake(t, []byte(`{"status":"Success","data":{}}`))
	c, err := NewClient(Config{Host: f.srv.URL, ClientID: testID, ClientSecret: testSecret, Production: false, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	is := issue(snap.Issues, provenance.NonProductionSource)
	if is == nil || is.Severity != provenance.Warning {
		t.Fatalf("issues = %+v", snap.Issues)
	}
	env, _ := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if env.Completeness.Status != provenance.Degraded || env.Completeness.CanAssertNone() {
		t.Errorf("a staging 'none' must not be assertable: %s", env.Completeness.Status)
	}
}

func TestFailuresAreErrorsNotEmptyResults(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad parameter", 400, `{"status":"Failure","message":"BAD_REQUEST: Location is not valid"}`, "Location is not valid"},
		{"server error", 503, `unavailable`, "HTTP 503"},
		{"200 but failure status", 200, `{"status":"Failure","message":"boom"}`, "boom"},
		{"200 not json", 200, `<html>maintenance</html>`, "not JSON"},
		{"200 empty", 200, ``, "not JSON"},
		{"200 no data field", 200, `{"status":"Success"}`, "no data"},
		{"200 null data", 200, `{"status":"Success","data":null}`, "no data"},
		{"data not object", 200, `{"status":"Success","data":[1]}`, "data is not an object"},
		{"geojson wrong type", 200, `{"status":"Success","data":{"geojson":"x"}}`, "neither a list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t, []byte(tt.body))
			f.apiStatus = tt.status
			snap, err := c.Query(context.Background(), q)
			if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrUpstream containing %q", err, tt.want)
			}
			if snap.Source.URL == "" || snap.Source.RetrievedAt.IsZero() || snap.Source.HTTPStatus != tt.status || len(snap.Items) != 0 {
				t.Errorf("failure must be attributable: %+v", snap.Source)
			}
		})
	}
}

func TestFeatureCollectionShapeAccepted(t *testing.T) {
	var d struct {
		Data struct {
			G []json.RawMessage `json:"geojson"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sample(t), &d); err != nil {
		t.Fatal(err)
	}
	fc, _ := json.Marshal(map[string]any{"status": "Success", "data": map[string]any{"geojson": map[string]any{"type": "FeatureCollection", "features": d.Data.G[:3]}}})
	_, c := newFake(t, fc)
	snap, err := c.Query(context.Background(), q)
	if err != nil || len(snap.Items) != 3 {
		t.Fatalf("items=%d err=%v", len(snap.Items), err)
	}
}

func TestBadRecordsAreFlaggedNotDropped(t *testing.T) {
	good := func(num string) string {
		return `{"type":"Feature","properties":{"coreNOTAMData":{"notam":{"id":"NMS_` + num + `","number":"` + num + `","type":"N","effectiveStart":"2026-11-01T00:00:00Z","effectiveEnd":"2026-12-01T00:00:00Z","text":"X","classification":"DOM","icaoLocation":"KIAD","minimumFl":"000","maximumFl":"999"},"notamTranslation":[{"type":"ICAO","formattedText":"RAW ` + num + `"}]}},"geometry":{"type":"GeometryCollection","geometries":[{"type":"Point","coordinates":[-77.4,38.9]}]}}`
	}
	body := `{"status":"Success","data":{"geojson":[` + good("01/001") + `,
	  {"properties":{"coreNOTAMData":{"notam":{"id":"NMS_BAD1","number":"09/001","effectiveStart":"yesterday","effectiveEnd":"later","type":"Z","coordinates":"nope","radius":"big","minimumFl":"abc"},"notamTranslation":[{"type":"ICAO","formattedText":"RAW BAD1"}]}},"geometry":null},
	  {"properties":{"coreNOTAMData":{"notam":{"id":"NMS_BAD2","number":"09/002","effectiveStart":"2026-11-01T00:00:00Z","effectiveEnd":"2026-12-01T00:00:00Z"},"notamTranslation":[]}},"geometry":{"type":"Polygon","coordinates":[[[1,1],[1,2],[2,2]]]}},
	  {"properties":"not an object"},
	  {"properties":{"coreNOTAMData":{"notam":{}}}},
	  {"properties":{"coreNOTAMData":{"notam":{"id":"NMS_BAD3","number":"09/003","effectiveStart":"2026-11-01T00:00:00Z","effectiveEnd":"2026-12-01T00:00:00Z"},"notamTranslation":[{"type":"ICAO","formattedText":"RAW BAD3"}]}},"geometry":{"type":"GeometryCollection","geometries":[{"type":"Point","coordinates":[-77,38]},{"type":"Polygon","coordinates":[[[1,1],[1,2],[2,2],[1,1]]]}]}}
	]}}`
	_, c := newFake(t, []byte(body))
	snap, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 6 || snap.Source.RecordsInFeed != 6 {
		t.Fatalf("items=%d feed=%d; nothing may be dropped", len(snap.Items), snap.Source.RecordsInFeed)
	}
	if g := byNumber(t, snap.Items, "01/001"); len(g.ParseIssues) != 0 || g.Geometry == nil {
		t.Errorf("good record damaged: %+v", g)
	}
	bad := byNumber(t, snap.Items, "09/001")
	joined := strings.Join(bad.ParseIssues, " | ")
	for _, want := range []string{"effectiveStart", "unrecognized NOTAM type", "Q-line coordinates", "radius", "Q-line lower"} {
		if !strings.Contains(joined, want) && !strings.Contains(strings.ToLower(joined), strings.ToLower(want)) {
			t.Errorf("missing issue %q in: %s", want, joined)
		}
	}
	if bad.RawText != "RAW BAD1" || bad.Type != "" {
		t.Errorf("raw text must survive and a bad type must not be kept: %+v", bad)
	}
	if g := byNumber(t, snap.Items, "09/002"); g.Geometry != nil || g.RawFormat != model.RawUpstreamJSON {
		t.Errorf("open ring must not become geometry; no text -> upstream json: %+v", g)
	}
	if g := byNumber(t, snap.Items, "09/003"); g.Geometry != nil {
		t.Errorf("mixed point+polygon must be refused, not guessed: %+v", g.Geometry)
	}
	stubs := 0
	for _, a := range snap.Items {
		if strings.HasPrefix(a.ID, SourceID+"#") {
			stubs++
			if a.RawFormat != model.RawUpstreamJSON || len(a.ParseIssues) == 0 {
				t.Errorf("stub = %+v", a)
			}
		}
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}
	if stubs != 2 {
		t.Errorf("stubs = %d, want 2", stubs)
	}
	if pf := issue(snap.Issues, provenance.ParseFailed); pf == nil || pf.Severity != provenance.Warning {
		t.Errorf("PARSE_FAILED = %+v", pf)
	}
	env, _ := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if env.Completeness.Status != provenance.Degraded {
		t.Errorf("status = %s", env.Completeness.Status)
	}
}

func TestTokenIsCachedAndRefreshed(t *testing.T) {
	now := fixedNow
	f, _ := newFake(t, []byte(`{"status":"Success","data":{}}`))
	c, _ := NewClient(Config{Host: f.srv.URL, ClientID: testID, ClientSecret: testSecret, Production: true, Now: func() time.Time { return now }})
	for i := 0; i < 3; i++ {
		if _, err := c.Query(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenCalls.Load() != 1 || f.apiCalls.Load() != 3 {
		t.Fatalf("token calls=%d api calls=%d; token must be reused", f.tokenCalls.Load(), f.apiCalls.Load())
	}
	now = fixedNow.Add(1799*time.Second - 30*time.Second) // inside the refresh margin
	if _, err := c.Query(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls.Load() != 2 {
		t.Errorf("token should refresh before expiry, calls=%d", f.tokenCalls.Load())
	}
}

func TestExpiredTokenIsRetriedOnce(t *testing.T) {
	f, c := newFake(t, []byte(`{"status":"Success","data":{}}`))
	f.rejectFirstToken = true
	if _, err := c.Query(context.Background(), q); err != nil {
		t.Fatalf("a rejected token should be replaced transparently: %v", err)
	}
	if f.tokenCalls.Load() != 2 || f.apiCalls.Load() != 2 {
		t.Errorf("token calls=%d api calls=%d", f.tokenCalls.Load(), f.apiCalls.Load())
	}
}

func TestAuthFailuresNeverLeakSecrets(t *testing.T) {
	// Wrong credentials.
	f, _ := newFake(t, nil)
	bad, _ := NewClient(Config{Host: f.srv.URL, ClientID: testID, ClientSecret: "wrong-secret-VALUE", Production: true})
	_, err := bad.Query(context.Background(), q)
	if !errors.Is(err, ErrAuth) || !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
	// API keeps rejecting even fresh tokens.
	f2, c2 := newFake(t, []byte(`{}`))
	f2.rejectFirstToken = true
	f2.tokenStatus = 200
	f2.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Write([]byte(`{"access_token":"tok-secret-ZZZ","expires_in":"1799"}`))
			return
		}
		w.WriteHeader(401)
	})
	snap, err2 := c2.Query(context.Background(), q)
	if !errors.Is(err2, ErrAuth) {
		t.Fatalf("err2 = %v", err2)
	}
	for _, e := range []error{err, err2} {
		for _, secret := range []string{testSecret, "wrong-secret-VALUE", "tok-secret-ZZZ", "Basic ", "Bearer "} {
			if strings.Contains(e.Error(), secret) {
				t.Errorf("error leaks %q: %v", secret, e)
			}
		}
	}
	for _, s := range []string{snap.Source.URL, snap.Source.ETag} {
		if strings.Contains(s, testSecret) || strings.Contains(s, "tok-secret-ZZZ") {
			t.Errorf("snapshot leaks a secret: %s", s)
		}
	}
	// Token endpoint transport failure must not echo request details.
	srv := httptest.NewServer(http.NotFoundHandler())
	host := srv.URL
	srv.Close()
	dead, _ := NewClient(Config{Host: host, ClientID: testID, ClientSecret: testSecret})
	if _, err := dead.Query(context.Background(), q); err == nil || strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), testID) {
		t.Errorf("transport error = %v", err)
	}
}

func TestQueryValidation(t *testing.T) {
	f64 := func(v float64) *float64 { return &v }
	t1, t2 := fixedNow, fixedNow.Add(time.Hour)
	ok := []Query{
		{Location: "KIAD"},
		{Latitude: f64(38.9), Longitude: f64(-77.4), RadiusNM: f64(25)},
		{EffectiveStart: &t1, EffectiveEnd: &t2},
		{Classification: "FDC", Location: "KIAD"},
		{NotamNumber: "08/224", Location: "KIAD"},
		{FreeText: "TWY"},
		{RadiusNM: f64(0), Latitude: f64(-90), Longitude: f64(180)},
	}
	for i, q := range ok {
		if err := q.validate(); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]Query{
		"empty":                  {},
		"lat without lon/radius": {Latitude: f64(38.9)},
		"lat+lon no radius":      {Latitude: f64(38.9), Longitude: f64(-77.4)},
		"radius too big":         {Latitude: f64(38.9), Longitude: f64(-77.4), RadiusNM: f64(101)},
		"radius negative":        {Latitude: f64(38.9), Longitude: f64(-77.4), RadiusNM: f64(-1)},
		"lat out of range":       {Latitude: f64(91), Longitude: f64(0), RadiusNM: f64(1)},
		"lon out of range":       {Latitude: f64(0), Longitude: f64(181), RadiusNM: f64(1)},
		"start without end":      {EffectiveStart: &t1},
		"end without start":      {EffectiveEnd: &t2},
		"end before start":       {EffectiveStart: &t2, EffectiveEnd: &t1},
		"number alone":           {NotamNumber: "08/224"},
		"classification alone":   {Classification: "DOM"},
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			if err := q.validate(); !errors.Is(err, ErrBadQuery) {
				t.Errorf("err = %v, want ErrBadQuery", err)
			}
		})
	}
	// Invalid queries must not touch the network.
	f, c := newFake(t, nil)
	if _, err := c.Query(context.Background(), Query{}); !errors.Is(err, ErrBadQuery) {
		t.Fatal(err)
	}
	if f.tokenCalls.Load() != 0 || f.apiCalls.Load() != 0 {
		t.Error("invalid query reached the network")
	}
}

func TestQueryParamsEncoding(t *testing.T) {
	f64 := func(v float64) *float64 { return &v }
	t1 := time.Date(2026, 11, 23, 12, 30, 0, 0, time.FixedZone("x", -5*3600))
	t2 := t1.Add(2 * time.Hour)
	v := Query{Latitude: f64(38.95), Longitude: f64(-77.45), RadiusNM: f64(25), EffectiveStart: &t1, EffectiveEnd: &t2, FreeText: "TWY CLSD"}.params()
	if v.Get("latitude") != "38.95" || v.Get("longitude") != "-77.45" || v.Get("radius") != "25" {
		t.Errorf("geo = %v", v)
	}
	if v.Get("effectiveStartDate") != "2026-11-23T17:30:00Z" || v.Get("effectiveEndDate") != "2026-11-23T19:30:00Z" {
		t.Errorf("times must be UTC: %v", v)
	}
	if v.Get("freeText") != "TWY CLSD" {
		t.Errorf("freeText = %q", v.Get("freeText"))
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, cfg := range []Config{
		{}, {Host: "not a url", ClientID: "a", ClientSecret: "b"}, {Host: "ftp://x", ClientID: "a", ClientSecret: "b"},
		{Host: "https://x", ClientSecret: "b"}, {Host: "https://x", ClientID: "a"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("%+v should be rejected", cfg)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("NMS_HOST", "https://example.test/")
	t.Setenv("NMS_CLIENT_ID", "id")
	t.Setenv("NMS_CLIENT_SECRET", "secret")
	t.Setenv("NMS_ENV", "")
	c, ok := ConfigFromEnv()
	if !ok || c.Host != "https://example.test" || c.Production {
		t.Errorf("cfg = %+v ok=%v (anything but NMS_ENV=production must be non-production)", c, ok)
	}
	t.Setenv("NMS_ENV", "Production")
	if c, _ := ConfigFromEnv(); !c.Production {
		t.Error("NMS_ENV=production not honored")
	}
	t.Setenv("NMS_CLIENT_SECRET", "")
	if _, ok := ConfigFromEnv(); ok {
		t.Error("missing secret must report not configured")
	}
}

func TestParseCoordAndLimits(t *testing.T) {
	if ll, ok := parseCoord("3856N07727W"); !ok || ll.Lat < 38.93 || ll.Lat > 38.94 || ll.Lon > -77.44 || ll.Lon < -77.46 {
		t.Errorf("coord = %+v ok=%v", ll, ok)
	}
	if ll, ok := parseCoord("3300S15100E"); !ok || ll.Lat > 0 || ll.Lon < 0 {
		t.Errorf("southern/eastern = %+v", ll)
	}
	for _, s := range []string{"", "3856N0772W", "3875N07727W", "9100N07727W", "3856X07727W", "3856N18100W"} {
		if _, ok := parseCoord(s); ok {
			t.Errorf("parseCoord(%q) should fail", s)
		}
	}
	cases := map[string]model.Datum{"SFC": model.DatumSFC, "unl": model.DatumUnl, "FL200": model.DatumFL, "6000FT AMSL": model.DatumMSL, "100FT AGL": model.DatumAGL, "100ft agl": model.DatumAGL}
	for s, want := range cases {
		if a, ok := parseLimit(s); !ok || a.Datum != want {
			t.Errorf("parseLimit(%q) = %+v %v", s, a, ok)
		}
	}
	for _, s := range []string{"135FT", "", "ABC", "FL", "6000 METERS"} {
		if _, ok := parseLimit(s); ok {
			t.Errorf("parseLimit(%q) should not parse (no datum)", s)
		}
	}
	if a, ok := parseQLineFL("000", false); !ok || a.Datum != model.DatumSFC {
		t.Errorf("000 lower = %+v", a)
	}
	if a, ok := parseQLineFL("999", true); !ok || a.Datum != model.DatumUnl {
		t.Errorf("999 upper = %+v", a)
	}
	if a, ok := parseQLineFL("000", true); !ok || a.Datum != model.DatumFL || *a.Value != 0 {
		t.Errorf("000 as an upper limit is FL000, not surface: %+v", a)
	}
}
