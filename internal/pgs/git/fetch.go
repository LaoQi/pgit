package git

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// FetchAuth 远程认证信息
type FetchAuth struct {
	Type     string // "none" | "basic"
	Username string
	Password string
	Proxy    string // HTTP 代理 URL（含 userinfo 时自动代理认证），如 http://user:pass@host:port
}

// FetchResult fetch 操作结果
type FetchResult struct {
	ObjectsWritten int  // 写入 loose store 的对象数
	RefsUpdated    int  // 创建/更新的 ref 数
	RefsDeleted    int  // 删除的 ref 数(本地有但远程无)
	UpToDate       bool // true = 无新对象(want 全被 have 覆盖)
	Wants          int  // want oid 数
	Haves          int  // have oid 数
	PackSize       int64 // pack 数据字节数
}

// FetchRemote 从 remoteURL 拉取仓库到 repoRoot（smart-http upload-pack 客户端）。
func FetchRemote(remoteURL, repoRoot string, auth *FetchAuth) (*FetchResult, error) {
	fetchStart := time.Now()
	remoteURL = strings.TrimRight(remoteURL, "/")
	client := &http.Client{Timeout: 5 * time.Minute}
	if auth != nil && auth.Proxy != "" {
		proxyURL, err := url.Parse(auth.Proxy)
		if err != nil {
			return nil, fmt.Errorf("fetch: invalid proxy URL: %w", err)
		}
		if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
			return nil, fmt.Errorf("fetch: proxy URL must be http or https, got %q", proxyURL.Scheme)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	remoteRefs, serverCaps, err := fetchInfoRefs(client, remoteURL, auth)
	if err != nil {
		log.Printf("fetch: info/refs failed: %v", err)
		return nil, err
	}

	if len(remoteRefs) == 0 {
		log.Printf("fetch: empty remote (no refs)")
		return &FetchResult{UpToDate: true}, nil
	}

	rs := NewRefStore(repoRoot)
	localRefsList, _ := rs.List()
	localRefs := make(map[string]Oid)
	localOidSet := make(map[Oid]bool)
	for _, r := range localRefsList {
		if r.Name == "HEAD" {
			continue
		}
		if !strings.HasPrefix(r.Name, "refs/") {
			continue
		}
		localRefs[r.Name] = r.Oid
		localOidSet[r.Oid] = true
	}

	var wantOids []Oid
	wantSet := make(map[Oid]bool)
	for name, oid := range remoteRefs {
		if name == "HEAD" {
			continue
		}
		if !wantSet[oid] {
			wantSet[oid] = true
			wantOids = append(wantOids, oid)
		}
	}

	allLocal := true
	for _, oid := range wantOids {
		if !localOidSet[oid] {
			allLocal = false
			break
		}
	}
	if allLocal {
		log.Printf("fetch: wants=%d haves=%d objects=0 (up-to-date)", len(wantOids), len(localOidSet))
		return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(localOidSet)}, nil
	}

	var haveOids []Oid
	haveSet := make(map[Oid]bool)
	for _, oid := range localRefs {
		if !haveSet[oid] {
			haveSet[oid] = true
			haveOids = append(haveOids, oid)
		}
	}

	clientCaps := "ofs-delta"
	if strings.Contains(serverCaps, "side-band-64k") {
		clientCaps = "side-band-64k ofs-delta"
	}
	useSideband := strings.Contains(clientCaps, "side-band-64k")

	var reqBuf bytes.Buffer
	pw := NewPktWriter(&reqBuf)
	for i, oid := range wantOids {
		if i == 0 {
			pw.WritePktString(fmt.Sprintf("want %s %s\n", oid, clientCaps))
		} else {
			pw.WritePktString(fmt.Sprintf("want %s\n", oid))
		}
	}
	pw.WriteFlush()
	for _, oid := range haveOids {
		pw.WritePktString(fmt.Sprintf("have %s\n", oid))
	}
	pw.WritePktString("done\n")

	postReq, err := http.NewRequest("POST", remoteURL+"/git-upload-pack", &reqBuf)
	if err != nil {
		return nil, fmt.Errorf("fetch: new upload-pack request: %w", err)
	}
	postReq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	postReq.Header.Set("Accept", "application/x-git-upload-pack-result")
	if auth != nil && auth.Type == "basic" {
		postReq.SetBasicAuth(auth.Username, auth.Password)
	}
	postResp, err := client.Do(postReq)
	if err != nil {
		log.Printf("fetch: upload-pack request failed: %v", err)
		return nil, fmt.Errorf("fetch: upload-pack request: %w", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		log.Printf("fetch: upload-pack status %d", postResp.StatusCode)
		return nil, fmt.Errorf("fetch: upload-pack status %d", postResp.StatusCode)
	}

	pr := NewPktReader(postResp.Body)
	firstPayload, isFlush, err := pr.ReadPkt()
	if err != nil {
		return nil, fmt.Errorf("fetch: read first response: %w", err)
	}
	if isFlush {
		return nil, fmt.Errorf("fetch: expected NAK/ACK, got flush")
	}
	firstLine := string(firstPayload)
	if firstLine != "NAK\n" && !strings.HasPrefix(firstLine, "ACK ") {
		return nil, fmt.Errorf("fetch: expected NAK or ACK, got %q", firstPayload)
	}

	var packBuf bytes.Buffer
	if useSideband {
		for {
			payload, isFlush, err := pr.ReadPkt()
			if err != nil {
				return nil, fmt.Errorf("fetch: read sideband: %w", err)
			}
			if isFlush {
				break
			}
			if len(payload) < 1 {
				continue
			}
			switch payload[0] {
			case SidebandPack:
				packBuf.Write(payload[1:])
			case SidebandProgress:
				log.Printf("remote: %s", string(payload[1:]))
			case SidebandError:
				return nil, fmt.Errorf("fetch: remote error: %s", string(payload[1:]))
			}
		}
	} else {
		remaining, err := io.ReadAll(postResp.Body)
		if err != nil {
			return nil, fmt.Errorf("fetch: read pack: %w", err)
		}
		if len(remaining) >= 4 && string(remaining[len(remaining)-4:]) == PktFlush {
			remaining = remaining[:len(remaining)-4]
		}
		packBuf.Write(remaining)
	}

	if packBuf.Len() == 0 {
		log.Printf("fetch: wants=%d haves=%d objects=0 (up-to-date)", len(wantOids), len(haveOids))
		return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(haveOids)}, nil
	}

	log.Printf("fetch: pack received, size=%d bytes", packBuf.Len())

	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	dec := NewPackDecoder(bytes.NewReader(packBuf.Bytes()), store)
	objs, err := dec.Decode()
	if err != nil {
		log.Printf("fetch: decode pack failed: %v", err)
		return nil, fmt.Errorf("fetch: decode pack: %w", err)
	}

	objectsWritten := 0
	for _, obj := range objs {
		oid := obj.Oid()
		if !oid.Valid() {
			return nil, fmt.Errorf("fetch: invalid oid for %s size %d", obj.Type, obj.Size)
		}
		if _, err := store.Write(obj); err != nil {
			return nil, fmt.Errorf("fetch: write %s: %w", oid, err)
		}
		objectsWritten++
	}

	var updates []RefUpdate
	for name, remoteOid := range remoteRefs {
		if name == "HEAD" {
			continue
		}
		if localOid, ok := localRefs[name]; ok {
			updates = append(updates, RefUpdate{Name: name, OldOid: localOid, NewOid: remoteOid})
		} else {
			updates = append(updates, RefUpdate{Name: name, OldOid: ZeroOid, NewOid: remoteOid})
		}
	}
	for name, localOid := range localRefs {
		if _, ok := remoteRefs[name]; !ok {
			updates = append(updates, RefUpdate{Name: name, OldOid: localOid, NewOid: ZeroOid})
		}
	}

	results, err := rs.Update(updates)
	if err != nil {
		return nil, fmt.Errorf("fetch: update refs: %w", err)
	}
	refsUpdated := 0
	refsDeleted := 0
	for i, u := range updates {
		if i < len(results) && results[i].Ok {
			if u.NewOid == ZeroOid {
				refsDeleted++
			} else {
				refsUpdated++
			}
		} else if i < len(results) {
			log.Printf("fetch: ref %s update failed: %s", u.Name, results[i].Reason)
		}
	}

	if headOid, ok := remoteRefs["HEAD"]; ok && headOid.Valid() && !headOid.IsZero() {
		for name, oid := range remoteRefs {
			if strings.HasPrefix(name, "refs/heads/") && oid == headOid {
				rs.SetHead(name)
				break
			}
		}
	} else {
		head, _ := rs.Head()
		if head != "" {
			if _, err := rs.Get(head); err != nil {
				currentRefs, _ := rs.List()
				for _, r := range currentRefs {
					if strings.HasPrefix(r.Name, "refs/heads/") {
						rs.SetHead(r.Name)
						break
					}
				}
			}
		}
	}

	duration := time.Since(fetchStart).Milliseconds()
	log.Printf("fetch: wants=%d haves=%d objects=%d packSize=%d duration=%dms",
		len(wantOids), len(haveOids), objectsWritten, packBuf.Len(), duration)

	return &FetchResult{
		ObjectsWritten: objectsWritten,
		RefsUpdated:    refsUpdated,
		RefsDeleted:    refsDeleted,
		UpToDate:       false,
		Wants:          len(wantOids),
		Haves:          len(haveOids),
		PackSize:       int64(packBuf.Len()),
	}, nil
}

// fetchInfoRefs 获取并解析 smart-http ref advertisement，返回 remoteRefs 与 serverCaps。
func fetchInfoRefs(client *http.Client, remoteURL string, auth *FetchAuth) (map[string]Oid, string, error) {
	req, err := http.NewRequest("GET", remoteURL+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: new info/refs request: %w", err)
	}
	req.Header.Set("Accept", "application/x-git-upload-pack-advertisement")
	if auth != nil && auth.Type == "basic" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: info/refs request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch: info/refs status %d", resp.StatusCode)
	}

	pr := NewPktReader(resp.Body)
	svc, isFlush, err := pr.ReadPkt()
	if err != nil {
		return nil, "", fmt.Errorf("fetch: read service frame: %w", err)
	}
	if isFlush {
		return nil, "", fmt.Errorf("fetch: unexpected flush for service frame")
	}
	if !strings.Contains(string(svc), "git-upload-pack") {
		return nil, "", fmt.Errorf("fetch: unexpected service frame %q", svc)
	}
	_, isFlush, err = pr.ReadPkt()
	if err != nil {
		return nil, "", fmt.Errorf("fetch: read flush after service: %w", err)
	}
	if !isFlush {
		return nil, "", fmt.Errorf("fetch: expected flush after service frame")
	}

	remoteRefs := make(map[string]Oid)
	var serverCaps string
	firstRef := true
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return nil, "", fmt.Errorf("fetch: read ref advertisement: %w", err)
		}
		if isFlush {
			break
		}
		line := string(payload)
		var main, capPart string
		if i := strings.IndexByte(line, 0); i >= 0 {
			main = line[:i]
			capPart = line[i+1:]
		} else {
			main = line
		}
		main = strings.TrimRight(main, "\n")
		if firstRef {
			serverCaps = strings.TrimRight(capPart, "\n")
			firstRef = false
		}
		if strings.Contains(main, "capabilities^{}") {
			continue
		}
		fields := strings.Fields(main)
		if len(fields) >= 2 {
			remoteRefs[fields[1]] = Oid(fields[0])
		}
	}
	return remoteRefs, serverCaps, nil
}
