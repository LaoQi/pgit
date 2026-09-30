package git

import "strings"

// LsRemote 只读取远端 ref 列表（smart-http ref advertisement），不传输任何对象。
//
// service 决定读取哪个广告视图：
//   - "git-upload-pack"：常规 ref 列表（首行含 HEAD）
//   - "git-receive-pack"：与推送视图一致的 ref 列表（不含 HEAD），
//     中转仓库校准上游基线、判断删除目标是否存在时使用
//
// 空字符串等同于 "git-upload-pack"。失败按 FetchOptions 的重试策略重试（仅可重试错误）。
func LsRemote(remoteURL string, auth *FetchAuth, opts FetchOptions, service string) (map[string]Oid, string, error) {
	if service == "" {
		service = "git-upload-pack"
	}
	o := opts.withDefaults()

	var lastErr error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		refs, caps, err := lsRemoteOnce(remoteURL, auth, o, service)
		if err == nil {
			return refs, caps, nil
		}
		lastErr = err
		if !isRetryableFetchError(err) || attempt == o.MaxAttempts {
			return nil, "", err
		}
		retrySleep(retryDelay(attempt, o.RetryBaseDelay, o.RetryMaxDelay))
	}
	return nil, "", lastErr
}

func lsRemoteOnce(remoteURL string, auth *FetchAuth, o FetchOptions, service string) (map[string]Oid, string, error) {
	session, err := newRemoteSession(auth, o)
	if err != nil {
		return nil, "", permanentErr("ls-remote: %v", err)
	}
	defer session.Close()
	return fetchRefAdvertisement(session.Ctx, session.Client, strings.TrimRight(remoteURL, "/"), service, auth, session.Touch)
}
