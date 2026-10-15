package cmd

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Source statistics remain separate from observations made by the downloader.
// nil means unknown, including a negative placeholder from an indexer.
type rescueCandidate struct {
	Hash       string    `json:"hash"`
	Title      string    `json:"title"`
	Magnet     string    `json:"magnet"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Size       int64     `json:"size"`
	Seeders    *int      `json:"reported_seeders"`
	Downloads  *int      `json:"reported_completed"`
	Trusted    bool      `json:"trusted"`
	Remake     bool      `json:"remake"`
	Labels     []string  `json:"labels,omitempty"`
	Score      int       `json:"initial_score"`
}

type torrentSearcher struct {
	client   *http.Client
	endpoint string
}

func newTorrentSearcher(opts torrentRescueOptions) *torrentSearcher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.ProxyURL != "" {
		proxy, _ := url.Parse(opts.ProxyURL)
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &torrentSearcher{client: &http.Client{Timeout: opts.SearchTimeout, Transport: transport}, endpoint: opts.RSSURL}
}

func (s *torrentSearcher) search(ctx context.Context, code string) ([]rescueCandidate, error) {
	u, _ := url.Parse(s.endpoint)
	u.RawQuery = url.Values{"page": {"rss"}, "q": {code}, "c": {"2_2"}, "f": {"0"}, "s": {"seeders"}, "o": {"desc"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 qbit-upload")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RSS 检索失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RSS 检索返回 HTTP %d", resp.StatusCode)
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType != "application/rss+xml" && contentType != "application/xml" && contentType != "text/xml" {
		return nil, fmt.Errorf("RSS 检索未返回 XML")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3_000_001))
	if err != nil {
		return nil, err
	}
	if len(body) > 3_000_000 {
		return nil, fmt.Errorf("RSS 响应超过 3MB")
	}
	return parseRescueRSS(body, code, time.Now().UTC())
}

func parseRescueRSS(data []byte, code string, now time.Time) ([]rescueCandidate, error) {
	var feed struct {
		XMLName xml.Name `xml:"rss"`
		Channel *struct {
			Items []struct {
				Title     string `xml:"title"`
				Hash      string `xml:"infoHash"`
				Size      string `xml:"size"`
				Seeders   string `xml:"seeders"`
				Downloads string `xml:"downloads"`
				Trusted   string `xml:"trusted"`
				Remake    string `xml:"remake"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil || feed.Channel == nil {
		return nil, fmt.Errorf("RSS 结构无效，可能是验证页面")
	}
	seen := map[string]bool{}
	out := []rescueCandidate{}
	for _, item := range feed.Channel.Items {
		parsed, ok := preprocessCatalogNumber(item.Title)
		if !ok || parsed.Code != code {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(item.Hash))
		if len(hash) != 40 || !validTorrentHash(hash) {
			return nil, fmt.Errorf("RSS 匹配条目缺少有效 infohash")
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		c := rescueCandidate{Hash: hash, Title: item.Title, Magnet: "magnet:?xt=urn:btih:" + hash, Source: "sukebei_rss", ObservedAt: now, Size: parseRescueSize(item.Size), Seeders: optionalRescueInt(item.Seeders), Downloads: optionalRescueInt(item.Downloads), Trusted: strings.EqualFold(item.Trusted, "yes"), Remake: strings.EqualFold(item.Remake, "yes"), Labels: rescueLabels(item.Title)}
		c.Score = initialRescueScore(c)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

func optionalRescueInt(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

var rescueSizeRE = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB|B)\s*$`)

func parseRescueSize(s string) int64 {
	m := rescueSizeRE.FindStringSubmatch(s)
	if len(m) != 3 {
		return 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	units := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KIB": 1024, "MIB": 1048576, "GIB": 1073741824, "TIB": 1099511627776}
	n *= units[strings.ToUpper(m[2])]
	if n <= 0 || n >= math.MaxInt64 {
		return 0
	}
	return int64(n)
}

var rescueSubtitleRE = regexp.MustCompile(`(?i)中文字幕|中字|字幕|CNSUB|CHINESE[ _-]*SUB`)

func rescueLabels(name string) []string {
	labels := []string{}
	if parsed, ok := preprocessCatalogNumber(name); ok {
		labels = append(labels, parsed.Tags...)
	}
	if quality4KTagRE.MatchString(strings.ToUpper(name)) {
		labels = append(labels, "4k")
	}
	if quality1080TagRE.MatchString(strings.ToUpper(name)) {
		labels = append(labels, "1080p")
	}
	if rescueSubtitleRE.MatchString(name) {
		labels = append(labels, "chinese-subtitles")
	}
	if regexp.MustCompile(`(?i)UNCENSORED|UNCEN\b|MOSAIC[ _-]*REMOVED|無碼|无码|破解`).MatchString(name) {
		labels = append(labels, "uncensored")
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, v := range labels {
		if v != "timestamp" && !seen[v] {
			unique = append(unique, v)
			seen[v] = true
		}
	}
	return unique
}
func initialRescueScore(c rescueCandidate) int {
	score := 20
	if c.Seeders != nil {
		score += int(math.Min(40, math.Log2(float64(*c.Seeders)+1)*8))
	}
	if c.Downloads != nil {
		score += int(math.Min(20, math.Log10(float64(*c.Downloads)+1)*5))
	}
	if c.Trusted {
		score += 10
	}
	if c.Size > 100*mb {
		score += 10
	}
	if c.Remake {
		score -= 10
	}
	return score
}

func newSearchTorrentsCmd() *cobra.Command {
	return &cobra.Command{Use: "search-torrents <catalog-number>", Short: "通过代理检索 RSS 候选并输出 JSON，不创建下载任务", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		opts, err := resolveTorrentRescueOptions(cfg, true)
		if err != nil {
			return err
		}
		parsed, ok := preprocessCatalogNumber(args[0])
		if !ok {
			return fmt.Errorf("无法识别番号")
		}
		results, err := newTorrentSearcher(opts).search(cmd.Context(), parsed.Code)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(results)
	}}
}
