package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

// The policy and event pagers are read-only views of one fetch each,
// refreshed on tick like explain.

// openFetched opens a pager whose content is fetch's, reloaded each tick.
func (m *Model) openFetched(title string, fetch func(ctx context.Context, b Backend) []string) tea.Cmd {
	p := m.newPager(title)
	gen, b := p.gen, m.backend
	p.reload = func() tea.Cmd {
		if b == nil {
			return nil
		}
		m.pager.loading = true
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return pagerMsg{gen: gen, lines: fetch(ctx, b)}
		}
	}
	return p.reload()
}

func (m *Model) openEgress(a *ateapi.Actor) tea.Cmd {
	md := a.GetMetadata()
	ref := &ateapi.ObjectRef{Atespace: md.GetAtespace(), Name: md.GetName()}
	return m.openFetched(stTitleKind.Render("egress")+" "+stTitleCount.Render(refID(ref)),
		func(ctx context.Context, b Backend) []string {
			p, err := b.GetActorEgressPolicy(ctx, ref)
			return renderEgress(ref, p, err, time.Now())
		})
}

// renderEgress shows an EgressPolicy's rules. Credential headers show the
// header name and the kind of source only: never the credential URI or the
// prefix.
func renderEgress(ref *ateapi.ObjectRef, p *ateapi.EgressPolicy, err error, now time.Time) []string {
	next := []string{"", stSection.Render("Next"),
		stDim.Render("$ ") + fg(cSky).Render("kubectl ate get egress-policy "+ref.GetName()+" -a "+ref.GetAtespace()+" -o yaml")}
	switch {
	case status.Code(err) == codes.NotFound:
		return append([]string{
			levelGlyph[explain.Error] + " " + fg(cRed).Render("no egress policy: all outbound traffic blocked"),
			"    " + stDim.Render("an actor without an EgressPolicy has all egress denied, and its DNS lookups answer NXDOMAIN"),
		}, next...)
	case err != nil:
		return []string{stStatusErr.Render("egress policy: " + errString(err))}
	}
	md := p.GetMetadata()
	out := []string{
		stSection.Render("EgressPolicy "+md.GetName()) + stDim.Render(fmt.Sprintf("  %d rule(s)  updated %s ago", len(p.GetRules()), explain.Age(md.GetUpdateTime().AsTime(), now))),
		stDim.Render("Rules only allow; traffic that matches no rule is refused. Only TCP is carried: UDP is blocked."),
		stDim.Render("For a name and port one rule decides: an exact hostname beats a wildcard, then listed ports beat all ports."),
		"",
	}
	if len(p.GetRules()) == 0 {
		out = append(out, levelGlyph[explain.Error]+" "+fg(cRed).Render("no rules: all outbound traffic blocked"))
	}
	for _, r := range p.GetRules() {
		out = append(out, egressRule(r)...)
	}
	return append(out, next...)
}

func egressRule(r *ateapi.EgressRule) []string {
	var proto, mode, ports string
	var hosts []string
	var effects *ateapi.HttpRuleEffects
	switch {
	case r.GetHttp() != nil:
		h := r.GetHttp()
		proto, mode, hosts, effects = "http", "cleartext, per request", h.GetHostnames(), h.GetEffects()
		ports = portsString(h.GetPorts(), "80")
	case r.GetHttps() != nil:
		h := r.GetHttps()
		proto, mode, hosts, effects = "https", "intercepted; the actor must trust the gateway CA", h.GetHostnames(), h.GetEffects()
		ports = portsString(h.GetPorts(), "443")
	case r.GetTlsPassthrough() != nil:
		t := r.GetTlsPassthrough()
		proto, mode, hosts = "tls_passthrough", "forwarded without decryption, on the SNI", t.GetHostnames()
		ports = portsString(t.GetPorts(), "")
	default:
		return []string{levelGlyph[explain.Warn] + " " + fg(cPeach).Render("rule of a protocol this client does not know")}
	}
	out := []string{
		levelGlyph[explain.OK] + " " + fg(cGreen).Bold(true).Render(fmt.Sprintf("%-16s", proto)) + stCount.Render(ports) + stDim.Render("  "+mode),
		"    " + stLabel.Render("hosts   ") + fg(cBlue).Render(strings.Join(hosts, ", ")),
	}
	for _, h := range effects.GetReplaceHeaders() {
		src := "credential"
		if h.GetActorJwt() != nil {
			src = "actor JWT"
		}
		line := "    " + stLabel.Render("inject  ") + fg(cMauve).Render(h.GetHeader()) + stDim.Render(" ← "+src+", when the request carries the header")
		out = append(out, line)
		if proto == "http" {
			out = append(out, "    "+levelGlyph[explain.Warn]+" "+fg(cPeach).Render("credential injected over cleartext HTTP; anything on the path can read it"))
		}
	}
	return out
}

// portsString applies the proto defaults: http 80, https 443, and
// tls_passthrough requires ports.
func portsString(p *ateapi.Ports, def string) string {
	switch {
	case p.GetAll() != nil:
		return "all ports"
	case len(p.GetNumbers()) > 0:
		var s []string
		for _, n := range p.GetNumbers() {
			s = append(s, fmt.Sprint(n))
		}
		if len(s) == 1 {
			return "port " + s[0]
		}
		return "ports " + strings.Join(s, ", ")
	case def != "":
		return "port " + def + " (default)"
	}
	return "no ports"
}

// accessSpace is one atespace's access policy, or why it has none.
type accessSpace struct {
	name   string
	policy *ateapi.AccessPolicy
	err    error
}

func (m *Model) openAccess() tea.Cmd {
	var spaces []string
	for _, a := range cachedObjs[*ateapi.Atespace](m, viewKey{kAtespaces, ""}) {
		spaces = append(spaces, a.GetMetadata().GetName())
	}
	return m.openFetched(stTitleKind.Render("access"), func(ctx context.Context, b Backend) []string {
		if spaces == nil {
			as, err := b.ListAtespaces(ctx)
			if err != nil {
				return []string{stStatusErr.Render("atespaces: " + errString(err))}
			}
			for _, a := range as {
				spaces = append(spaces, a.GetMetadata().GetName())
			}
		}
		var global *ateapi.AccessPolicy
		var gerr error
		out := make([]accessSpace, len(spaces))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		wg.Add(1)
		go func() {
			defer wg.Done()
			global, gerr = b.GetGlobalAccessPolicy(ctx)
		}()
		for i, s := range spaces {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				p, err := b.GetAtespaceAccessPolicy(ctx, s)
				out[i] = accessSpace{name: s, policy: p, err: err}
			}()
		}
		wg.Wait()
		return renderAccess(global, gerr, out)
	})
}

// renderAccess lists role bindings. ateapi has no can-i RPC, so this shows
// who is bound to what and predicts nothing.
func renderAccess(global *ateapi.AccessPolicy, gerr error, spaces []accessSpace) []string {
	out := []string{
		stDim.Render("Role bindings as stored. ateapi has no can-i RPC, so these are not anyone's effective permissions."),
		"",
		stSection.Render("Global"),
	}
	switch {
	case status.Code(gerr) == codes.NotFound:
		out = append(out, "  "+stDim.Render("no global access policy yet; the server's bootstrap owners are global owners regardless"))
	case gerr != nil:
		out = append(out, "  "+stStatusErr.Render(errString(gerr)))
	default:
		out = append(out, bindingLines(global.GetBindings())...)
		out = append(out, "  "+stDim.Render("the server's bootstrap owners are global owners too, whether listed or not"))
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].name < spaces[j].name })
	for _, s := range spaces {
		head := stSection.Render("Atespace ") + stAtespace.Render(s.name)
		switch {
		case status.Code(s.err) == codes.NotFound:
			out = append(out, "", head+stDim.Render("  no access policy"))
		case s.err != nil:
			out = append(out, "", head+"  "+stStatusErr.Render(errString(s.err)))
		default:
			out = append(out, "", head)
			out = append(out, bindingLines(s.policy.GetBindings())...)
		}
	}
	return out
}

func bindingLines(bs []*ateapi.Binding) []string {
	if len(bs) == 0 {
		return []string{"  " + stDim.Render("no bindings")}
	}
	w := 0
	for _, b := range bs {
		w = max(w, len(b.GetRole()))
	}
	var out []string
	for _, b := range bs {
		role := fg(cPeach).Render(fmt.Sprintf("%-*s", w+2, b.GetRole()))
		if len(b.GetMembers()) == 0 {
			out = append(out, "  "+role+stDim.Render("no members"))
		}
		for i, mem := range b.GetMembers() {
			if i > 0 {
				role = strings.Repeat(" ", w+2)
			}
			out = append(out, "  "+role+fg(cBlue).Render(mem))
		}
	}
	return out
}

// openEvents lists the Kubernetes events for a worker pod.
func (m *Model) openEvents(namespace, pod string) tea.Cmd {
	return m.openFetched(stTitleKind.Render("events")+" "+stTitleCount.Render(namespace+"/"+pod),
		func(ctx context.Context, b Backend) []string {
			evs, err := b.PodEvents(ctx, namespace, pod)
			if err != nil {
				return []string{stStatusErr.Render("events: " + errString(err))}
			}
			return renderEvents(evs, time.Now())
		})
}

// renderEvents is a kubectl-style event table, newest first.
func renderEvents(evs []corev1.Event, now time.Time) []string {
	if len(evs) == 0 {
		return []string{stDim.Render("no events; Kubernetes keeps events for about an hour")}
	}
	sorted := make([]*corev1.Event, len(evs))
	for i := range evs {
		sorted[i] = &evs[i]
	}
	sort.SliceStable(sorted, func(i, j int) bool { return explain.EventTime(sorted[i]).After(explain.EventTime(sorted[j])) })
	rw := len("REASON")
	for _, e := range sorted {
		rw = max(rw, len(e.Reason))
	}
	out := []string{stColHeader.Render(fmt.Sprintf("%-6s %-8s %-*s %5s  %s", "AGE", "TYPE", rw, "REASON", "COUNT", "MESSAGE"))}
	for _, e := range sorted {
		typ := fg(cGreen).Render(fmt.Sprintf("%-8s", e.Type))
		msg := fg(cText).Render(e.Message)
		if e.Type == corev1.EventTypeWarning {
			typ = fg(cRed).Bold(true).Render(fmt.Sprintf("%-8s", e.Type))
			msg = fg(cRed).Render(e.Message)
		}
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf("x%d", e.Count)
		}
		out = append(out, stDim.Render(fmt.Sprintf("%-6s ", explain.Age(explain.EventTime(e), now)))+typ+" "+
			fg(cBlue).Render(fmt.Sprintf("%-*s", rw, e.Reason))+stDim.Render(fmt.Sprintf(" %5s  ", count))+msg)
	}
	return out
}
