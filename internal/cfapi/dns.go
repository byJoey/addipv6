package cfapi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Identity 是凭据校验的结果。两种认证方式拿到的字段不一样，
// Token 有 status，全局 Key 有邮箱。
type Identity struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	Email  string `json:"email,omitempty"`
}

// Zone 是一个 Cloudflare 站点。
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Record 是一条 DNS 记录。
type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

// Managed 判断这条记录是不是本工具建的。
func (r Record) Managed() bool { return strings.TrimSpace(r.Comment) == RecordComment }

// Verify 校验凭据能不能用。
//
// 两种认证打的端点不一样：Token 走 /user/tokens/verify，
// 全局 Key 走 /user（Token 打 /user 会被 403 挡掉，反过来也不行）。
func (c *Client) Verify(ctx context.Context) (Identity, error) {
	if c.Cred.Mode == ModeGlobalKey {
		var out Identity
		if _, err := c.do(ctx, "GET", "/user", nil, nil, &out); err != nil {
			return Identity{}, err
		}
		if out.Email != "" && !strings.EqualFold(out.Email, c.Cred.Email) {
			return out, fmt.Errorf("这把 Key 对应的邮箱是 %s，跟你填的对不上", out.Email)
		}
		return out, nil
	}

	var out Identity
	if _, err := c.do(ctx, "GET", "/user/tokens/verify", nil, nil, &out); err != nil {
		return Identity{}, err
	}
	if out.Status != "" && out.Status != "active" {
		return out, fmt.Errorf("Token 状态是 %s，不可用", out.Status)
	}
	return out, nil
}

// ListZones 列出 Token 能看到的全部站点，自动翻页。
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var all []Zone
	for page := 1; page <= 50; page++ {
		q := url.Values{}
		q.Set("per_page", "50")
		q.Set("page", strconv.Itoa(page))
		var batch []Zone
		env, err := c.do(ctx, "GET", "/zones", q, nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if env.ResultInfo == nil || env.ResultInfo.TotalPages <= page || len(batch) == 0 {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// ListRecords 列出指定 zone 的记录，recordType 为空表示不过滤。
func (c *Client) ListRecords(ctx context.Context, zoneID, recordType string) ([]Record, error) {
	if zoneID == "" {
		return nil, errors.New("没选站点")
	}
	var all []Record
	for page := 1; page <= 200; page++ {
		q := url.Values{}
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		if recordType != "" {
			q.Set("type", recordType)
		}
		var batch []Record
		env, err := c.do(ctx, "GET", "/zones/"+url.PathEscape(zoneID)+"/dns_records", q, nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if env.ResultInfo == nil || env.ResultInfo.TotalPages <= page || len(batch) == 0 {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].Content < all[j].Content
	})
	return all, nil
}

// CreateRequest 是建一条记录需要的字段。
type CreateRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

// CreateRecord 建一条记录。带 comment 被拒时会去掉 comment 再试一次，
// 免得某些 Token 权限或套餐不支持注释导致整批失败。
func (c *Client) CreateRecord(ctx context.Context, zoneID string, req CreateRequest) (Record, error) {
	var out Record
	_, err := c.do(ctx, "POST", "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, req, &out)
	if err != nil && req.Comment != "" && commentRejected(err) {
		retry := req
		retry.Comment = ""
		_, err = c.do(ctx, "POST", "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, retry, &out)
	}
	if err != nil {
		return Record{}, err
	}
	return out, nil
}

func commentRejected(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Error()), "comment")
}

// DeleteRecord 删一条记录。
func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, "DELETE", "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil, nil, nil)
	return err
}

// BatchItem 是批量操作里的一行结果。
type BatchItem struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	ID      string `json:"id,omitempty"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// BatchResult 汇总一次批量操作。
type BatchResult struct {
	Items   []BatchItem `json:"items"`
	Created int         `json:"created"`
	Deleted int         `json:"deleted"`
	Skipped int         `json:"skipped"`
	Failed  int         `json:"failed"`
}

// batchConcurrency 是批量操作的并发度，配合 4 次/秒的限速足够快又不会触顶。
const batchConcurrency = 4

// BatchCreateAAAA 把一批 IPv6 地址写成 AAAA 记录。
// 同名同内容的记录已存在时跳过，所以可以放心重复点。
func (c *Client) BatchCreateAAAA(ctx context.Context, zoneID string, names []string, addrs []netip.Addr, ttl int, proxied bool) (*BatchResult, error) {
	if zoneID == "" {
		return nil, errors.New("没选站点")
	}
	if len(names) != len(addrs) {
		return nil, errors.New("名称和地址数量对不上")
	}
	if len(addrs) == 0 {
		return nil, errors.New("没有要解析的地址")
	}
	if ttl <= 0 {
		ttl = 60
	}

	existing, err := c.ListRecords(ctx, zoneID, "AAAA")
	if err != nil {
		return nil, fmt.Errorf("拉取现有 AAAA 记录失败: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		have[recordKey(r.Name, r.Content)] = struct{}{}
	}

	res := &BatchResult{Items: make([]BatchItem, len(addrs))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan int)

	worker := func() {
		defer wg.Done()
		for i := range jobs {
			name := strings.ToLower(strings.TrimSpace(names[i]))
			content := addrs[i].StringExpanded()
			short := addrs[i].String()
			item := BatchItem{Name: name, Content: short}

			mu.Lock()
			_, dup := have[recordKey(name, content)]
			if !dup {
				_, dup = have[recordKey(name, short)]
			}
			if !dup {
				// 先占位，避免同一批里两个相同项被并发重复创建。
				have[recordKey(name, content)] = struct{}{}
			}
			mu.Unlock()

			if dup {
				item.OK = true
				item.Skipped = true
				mu.Lock()
				res.Items[i] = item
				res.Skipped++
				mu.Unlock()
				continue
			}

			rec, err := c.CreateRecord(ctx, zoneID, CreateRequest{
				Type:    "AAAA",
				Name:    name,
				Content: short,
				TTL:     ttl,
				Proxied: proxied,
				Comment: RecordComment,
			})
			mu.Lock()
			if err != nil {
				item.Error = err.Error()
				res.Failed++
				// 创建失败就把占位撤掉，方便重试。
				delete(have, recordKey(name, content))
			} else {
				item.OK = true
				item.ID = rec.ID
				res.Created++
			}
			res.Items[i] = item
			mu.Unlock()
		}
	}

	for i := 0; i < batchConcurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for i := range addrs {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return res, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return res, nil
}

// BatchDelete 按记录 ID 批量删。
func (c *Client) BatchDelete(ctx context.Context, zoneID string, records []Record) (*BatchResult, error) {
	if zoneID == "" {
		return nil, errors.New("没选站点")
	}
	if len(records) == 0 {
		return nil, errors.New("没有要删的记录")
	}
	res := &BatchResult{Items: make([]BatchItem, len(records))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan int)

	worker := func() {
		defer wg.Done()
		for i := range jobs {
			r := records[i]
			item := BatchItem{Name: r.Name, Content: r.Content, ID: r.ID}
			err := c.DeleteRecord(ctx, zoneID, r.ID)
			mu.Lock()
			if err != nil {
				var apiErr *APIError
				// 81044 = 记录不存在，说明已经被删过，按成功处理。
				if errors.As(err, &apiErr) && (apiErr.Code(81044) || apiErr.Status == 404) {
					item.OK = true
					item.Skipped = true
					res.Skipped++
				} else {
					item.Error = err.Error()
					res.Failed++
				}
			} else {
				item.OK = true
				res.Deleted++
			}
			res.Items[i] = item
			mu.Unlock()
		}
	}

	for i := 0; i < batchConcurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for i := range records {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return res, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return res, nil
}

func recordKey(name, content string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".")) + "|" + strings.ToLower(strings.TrimSpace(content))
}
