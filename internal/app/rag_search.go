package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// SearchResult is a stripped, tracker-free web snippet fetched in Privacy Web mode.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// RetrieveVaultContext searches the user's encrypted Knowledge Vault documents
// in RAM and returns the most relevant snippets for local RAG.
func RetrieveVaultContext(docs []DocItem, query string, maxChars int) (string, []string) {
	if len(docs) == 0 || strings.TrimSpace(query) == "" {
		return "", nil
	}
	terms := tokenizeQuery(query)
	type scoredChunk struct {
		docName string
		text    string
		score   int
	}
	var chunks []scoredChunk
	for _, d := range docs {
		paras := splitChunks(d.Content, 600)
		for _, p := range paras {
			lower := strings.ToLower(p)
			score := 0
			for _, t := range terms {
				if strings.Contains(lower, t) {
					score += 3
				}
			}
			if strings.Contains(strings.ToLower(d.Name), strings.ToLower(query)) {
				score += 4
			}
			// If user only has 1-2 small docs, include them even on general questions
			if score == 0 && len(docs) <= 2 && len(d.Content) < 2000 {
				score = 1
			}
			if score > 0 {
				chunks = append(chunks, scoredChunk{docName: d.Name, text: p, score: score})
			}
		}
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].score > chunks[j].score })
	if len(chunks) == 0 {
		return "", nil
	}
	var sb strings.Builder
	var sources []string
	seenSrc := map[string]bool{}
	sb.WriteString("[KNOWLEDGE VAULT — PRIVATE LOCAL DOCUMENTS]\n")
	for _, c := range chunks {
		entry := fmt.Sprintf("--- From %s ---\n%s\n\n", c.docName, c.text)
		if sb.Len()+len(entry) > maxChars {
			break
		}
		sb.WriteString(entry)
		if !seenSrc[c.docName] {
			seenSrc[c.docName] = true
			sources = append(sources, "Vault: "+c.docName)
		}
	}
	return sb.String(), sources
}

func tokenizeQuery(q string) []string {
	words := regexp.MustCompile(`[a-zA-Z0-9]{3,}`).FindAllString(strings.ToLower(q), -1)
	stop := map[string]bool{"the": true, "and": true, "for": true, "with": true, "what": true, "how": true, "from": true, "that": true, "this": true, "can": true, "you": true, "about": true}
	var out []string
	for _, w := range words {
		if !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

func splitChunks(s string, maxLen int) []string {
	raw := strings.Split(s, "\n\n")
	var out []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for len(p) > maxLen {
			out = append(out, p[:maxLen])
			p = p[maxLen:]
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	reResultBlock = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	reHTMLTags    = regexp.MustCompile(`<[^>]+>`)
)

// PrivacyWebSearch sends ONLY the anonymized search query (never chat history)
// through the NetGuard client when ModeWeb is enabled.
func PrivacyWebSearch(guard *NetGuard, query string) ([]SearchResult, error) {
	if guard.Mode() != ModeWeb {
		return nil, fmt.Errorf("web search requires 'Privacy Web' network mode (currently in %s mode)", guard.Mode())
	}
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; USB-AI-PrivacyProxy/1.0)")
	resp, err := guard.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	matches := reResultBlock.FindAllStringSubmatch(string(body), 4)
	var out []SearchResult
	for _, m := range matches {
		out = append(out, SearchResult{
			URL:     cleanHTML(m[1]),
			Title:   cleanHTML(m[2]),
			Snippet: cleanHTML(m[3]),
		})
	}
	return out, nil
}

func cleanHTML(s string) string {
	s = reHTMLTags.ReplaceAllString(s, "")
	r := strings.NewReplacer("&amp;", "&", "&quot;", `"`, "&#x27;", "'", "&lt;", "<", "&gt;", ">", "&nbsp;", " ")
	return strings.TrimSpace(r.Replace(s))
}
