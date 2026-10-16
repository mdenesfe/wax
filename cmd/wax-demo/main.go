// Command wax-demo serves the admin interface with made-up data and no
// WhatsApp connection or database, for trying the interface, developing
// it, and taking screenshots.
//
//	go run ./cmd/wax-demo            # http://127.0.0.1:8090
//
// Sign in with any key for the tenant view, or with "admin" for the
// admin view. The demo is read-only, except that sent text messages
// appear in the conversation until the process exits.
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/webui"
)

// voiceWebM is a short tone recorded as WebM Opus (as browsers record).
//
//go:embed voice.webm
var voiceWebM []byte

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to listen on")
	flag.Parse()
	voice, _, err := media.WebMToOgg(voiceWebM)
	if err != nil {
		log.Fatal(err)
	}
	d := &demo{accounts: []*account{
		buildSupport(voice),
		buildMarketing(voice),
		{id: "sales", status: "qr", qr: true},
		{id: "billing", status: "logged_out"},
	}, pictures: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.Handle("GET /ui/", http.StripPrefix("/ui", webui.Handler()))
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	mux.Handle("/v1/", d.routes())
	log.Printf("wax demo on http://%s/ (any key signs in; %q for the admin view)", *listen, "admin")
	log.Fatal(http.ListenAndServe(*listen, mux))
}

type demo struct {
	mu       sync.Mutex
	accounts []*account
	pictures map[string][]byte
	outbox   int
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// routes is the subset of the REST API the interface uses.
func (d *demo) routes() http.Handler {
	m := http.NewServeMux()
	admin := func(h http.HandlerFunc) http.HandlerFunc { return d.auth(true, h) }
	tenant := func(h func(http.ResponseWriter, *http.Request, *account)) http.HandlerFunc {
		return d.auth(false, func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if id := r.PathValue("id"); id != "" {
				a := d.account(id)
				if a == nil {
					fail(w, http.StatusNotFound, "no such instance")
					return
				}
				h(w, r, a)
				return
			}
			h(w, r, nil)
		})
	}
	m.HandleFunc("GET /v1/tenants", admin(d.tenants))
	m.HandleFunc("GET /v1/tenants/{tid}", admin(d.tenant))
	m.HandleFunc("GET /v1/tenants/{tid}/keys", admin(d.keys))
	m.HandleFunc("GET /v1/instances", tenant(d.instances))
	m.HandleFunc("GET /v1/instances/{id}", tenant(d.instance))
	m.HandleFunc("PATCH /v1/instances/{id}", tenant(d.instance))
	m.HandleFunc("GET /v1/instances/{id}/qr", tenant(d.qr))
	m.HandleFunc("GET /v1/instances/{id}/chats", tenant(d.chats))
	m.HandleFunc("GET /v1/instances/{id}/chats/{chat}/messages", tenant(d.messages))
	m.HandleFunc("GET /v1/instances/{id}/chats/{chat}/messages/{mid}/media", tenant(d.media))
	m.HandleFunc("GET /v1/instances/{id}/chats/{chat}/picture", tenant(d.picture))
	m.HandleFunc("GET /v1/instances/{id}/chats/{chat}/profile", tenant(d.profile))
	m.HandleFunc("GET /v1/instances/{id}/groups", tenant(d.groupList))
	m.HandleFunc("GET /v1/instances/{id}/groups/{group}", tenant(d.groupInfo))
	m.HandleFunc("POST /v1/instances/{id}/messages", tenant(d.send))
	m.HandleFunc("GET /v1/instances/{id}/outbox/{oid}", tenant(func(w http.ResponseWriter, _ *http.Request, _ *account) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
	}))
	m.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fail(w, http.StatusForbidden, "The demo is read-only")
	})
	return m
}

// auth accepts any key: "admin" is the admin, anything else a tenant.
func (d *demo) auth(adminOnly bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case !ok || key == "":
			fail(w, http.StatusUnauthorized, "missing API key")
		case (key == "admin") != adminOnly:
			fail(w, http.StatusForbidden, "not allowed with this key")
		default:
			h(w, r)
		}
	}
}

func (d *demo) account(id string) *account {
	for _, a := range d.accounts {
		if a.id == id {
			return a
		}
	}
	return nil
}

// --- admin ---------------------------------------------------------------------------

var created = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

var tenantList = []map[string]any{
	{"id": "acme", "name": "Acme Inc.", "max_instances": 10, "requests_per_second": 20, "created_at": created},
	{"id": "globex", "name": "Globex", "max_instances": 3, "requests_per_second": 0, "created_at": created.AddDate(0, 1, 4)},
	{"id": "initech", "name": "Initech", "max_instances": 0, "requests_per_second": 5, "created_at": created.AddDate(0, 2, 11)},
}

func (d *demo) tenants(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, tenantList)
}

func (d *demo) tenant(w http.ResponseWriter, r *http.Request) {
	for _, t := range tenantList {
		if t["id"] == r.PathValue("tid") {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	fail(w, http.StatusNotFound, "no such tenant")
}

func (d *demo) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]any{
		{"id": "0a1b2c3d4e5f6071", "created_at": created},
		{"id": "8f9e0d1c2b3a4958", "created_at": created.AddDate(0, 4, 0)},
	})
}

// --- accounts ------------------------------------------------------------------------

func (a *account) view() map[string]any {
	v := map[string]any{"id": a.id, "status": a.status, "rate_per_minute": 20, "qr_pending": a.qr}
	if a.jid != "" {
		v["jid"], v["lid"] = a.jid, a.lid
	}
	if a.webhook != "" {
		v["webhook_url"] = a.webhook
	}
	return v
}

func (d *demo) instances(w http.ResponseWriter, _ *http.Request, _ *account) {
	out := make([]map[string]any, len(d.accounts))
	for i, a := range d.accounts {
		out[i] = a.view()
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *demo) instance(w http.ResponseWriter, _ *http.Request, a *account) {
	writeJSON(w, http.StatusOK, a.view())
}

func (d *demo) qr(w http.ResponseWriter, _ *http.Request, a *account) {
	if !a.qr {
		fail(w, http.StatusNotFound, "no QR code pending")
		return
	}
	png, err := pairing.RenderPNG("2@wax-demo-qr-code-not-for-scanning," + a.id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

// --- chats and messages --------------------------------------------------------------

func (d *demo) chats(w http.ResponseWriter, _ *http.Request, a *account) {
	if a.chats == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, a.chats)
}

// messages lists newest first, like the API.
func (d *demo) messages(w http.ResponseWriter, r *http.Request, a *account) {
	all := a.msgs[r.PathValue("chat")]
	var types []string
	if t := r.URL.Query().Get("types"); t != "" {
		types = strings.Split(t, ",")
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	out := []manager.MessageJSON{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if len(types) == 0 || slices.Contains(types, all[i].Type) {
			out = append(out, all[i])
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *demo) media(w http.ResponseWriter, r *http.Request, a *account) {
	f, ok := a.media[r.PathValue("mid")]
	if !ok {
		fail(w, http.StatusNotFound, "no media")
		return
	}
	w.Header().Set("Content-Type", f.contentType)
	_, _ = w.Write(f.data)
}

// send shows a sent text message in the conversation (nothing leaves).
func (d *demo) send(w http.ResponseWriter, r *http.Request, a *account) {
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		fail(w, http.StatusForbidden, "The demo only shows sent text messages")
		return
	}
	d.outbox++
	b := &builder{a: a, c: newClock(), n: 900 + d.outbox}
	m := b.msg(req.To, "", time.Now(), req.Text)
	for _, c := range a.chats {
		if c.JID == req.To {
			last := *m
			c.LastMessage, c.LastMessageAt = &last, &last.Timestamp
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"outbox_id": d.outbox, "message_id": m.ID, "status": "pending"})
}

// --- people and groups ---------------------------------------------------------------

func (d *demo) picture(w http.ResponseWriter, r *http.Request, _ *account) {
	jid := r.PathValue("chat")
	pic, ok := d.pictures[jid]
	if !ok {
		switch {
		case groupByJID(jid) != nil:
			pic = groupPicture(jid)
		case strings.HasPrefix(jid, "90555000000") || strings.HasPrefix(jid, "10000000000"):
			pic = personPicture("me")
		default:
			for _, p := range people {
				if (p.lid() == jid || p.phone()+"@s.whatsapp.net" == jid) && !p.noPicture {
					pic = personPicture(p.key)
				}
			}
		}
		d.pictures[jid] = pic
	}
	if pic == nil {
		fail(w, http.StatusNotFound, "no profile picture")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(pic)
}

func counts(a *account, chat string) map[string]int {
	out := map[string]int{}
	for _, m := range a.msgs[chat] {
		out[m.Type]++
	}
	return out
}

func (d *demo) profile(w http.ResponseWriter, r *http.Request, a *account) {
	jid := r.PathValue("chat")
	out := map[string]any{"jid": jid, "is_business": false, "live": true, "about_hidden": false,
		"common_groups": []map[string]string{}, "messages": counts(a, jid)}
	for _, p := range people {
		if p.lid() != jid {
			continue
		}
		out["phone"], out["lid"], out["push_name"], out["devices"] = p.phone(), strings.TrimSuffix(p.lid(), "@lid"), p.push, 2
		if p.name != "" {
			out["name"] = p.name
		}
		if p.username != "" {
			out["username"] = p.username
		}
		if p.about != "" {
			out["about"], out["about_set_at"] = p.about, created.AddDate(0, 1, 0)
		} else {
			out["about_hidden"] = true
		}
		if p.business {
			out["is_business"] = true
			out["business"] = map[string]any{
				"categories": []string{"Bakery"},
				"fields": map[string][]string{
					"description": {"Sourdough, pastries and coffee. Orders for pickup by WhatsApp."},
					"address":     {"12 Market Street"},
					"email":       {"hello@example.com"},
					"website":     {"https://example.com"},
				},
				"time_zone": "Europe/Istanbul",
				"hours": []map[string]string{
					{"day": "mon", "mode": "specific_hours", "open": "420", "close": "1140"},
					{"day": "sat", "mode": "specific_hours", "open": "480", "close": "960"},
					{"day": "sun", "mode": "appointment_only"},
				},
			}
		}
		var common []map[string]string
		for _, g := range groups {
			if !g.isCommunity && slices.Contains(g.members, p.key) {
				common = append(common, map[string]string{"jid": g.jid, "name": g.name})
			}
		}
		if common != nil {
			out["common_groups"] = common
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *demo) groupList(w http.ResponseWriter, _ *http.Request, _ *account) {
	out := make([]map[string]any, len(groups))
	for i, g := range groups {
		out[i] = map[string]any{"jid": g.jid, "name": g.name, "topic": g.topic, "is_community": g.isCommunity,
			"community": g.community, "default_sub_group": g.announcements, "announce": g.announce, "participants": len(g.members) + 1}
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *demo) groupInfo(w http.ResponseWriter, r *http.Request, _ *account) {
	g := groupByJID(r.PathValue("group"))
	if g == nil {
		fail(w, http.StatusNotFound, "group not found")
		return
	}
	owner := who(g.admins[0])
	parts := []map[string]any{{"jid": meLID, "phone_number": me, "is_admin": false, "is_super_admin": false, "phone": "905550000001"}}
	for _, k := range g.members {
		p := who(k)
		e := map[string]any{"jid": p.lid(), "phone_number": p.phone() + "@s.whatsapp.net", "is_admin": slices.Contains(g.admins, k),
			"is_super_admin": k == g.admins[0], "phone": p.phone(), "push_name": p.push}
		if p.name != "" {
			e["name"] = p.name
		}
		parts = append(parts, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jid": g.jid, "name": g.name, "topic": g.topic, "addressing_mode": "lid",
		"created": created, "owner": owner.lid(), "owner_pn": owner.phone() + "@s.whatsapp.net", "size": len(parts),
		"name_set_at": created.AddDate(0, 0, 3), "name_set_by": owner.lid(),
		"topic_set_at": created.AddDate(0, 0, 5), "topic_set_by": owner.lid(),
		"ephemeral": 0, "member_add_mode": "admin_add", "join_approval": g.isCommunity, "suspended": false,
		"announce": g.announce, "locked": true, "is_community": g.isCommunity, "community": g.community,
		"default_sub_group": g.announcements, "participants": parts,
	})
}
