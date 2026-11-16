# mcp-discord-roleplay — 設計 **v3（定稿候選）**

- 2026-11-16　v0→v1→v2→**v3**。v3：C1/C3/C4 拍定；**C2 我原先的說法是錯的，已改正**（4 MiB 是「入口」限制，出口 streaming 無上限）
- 狀態：**設計定案（v3.1），已依此動工**。本檔是實作的依據，與 `_state/` 的討論稿同步。
- 實作進度：**M0 完成**（config / router / bus / discord gateway client / `discord_status` / `--selftest`）；M1 = `discord_send`，M2 = `discord_poll` + media，M3 = history/channels，M4 = README。
- 建置驗證由 user 執行（sandbox 內無 shell / 無 go）：見 `_state/build-notes.md`。
- 環境：**Go 1.27.4**（已確認）、單一 bot token、client = 自研 agent、測試由 user 直接把 token/指令接進 session
- 依賴：`github.com/bwmarrin/discordgo v0.29.0`（API 簽名已逐條對过原始碼）

## 定案總表
| 項目 | 決定 |
|---|---|
| poll 語義 | **等 max_wait 秒；期間一有新訊息立刻 return**（不累積、不等安靜期）。`0` 不合法，有**下限**（提議 5s），超範圍 → `isError` + 回報合法區間 |
| 砍掉 | `debounce_ms`、`min_count`（與「有訊即回」衝突；同瞬間的多則自然會一次帶回）、`reply_to`、`thread_id`、`silent`、`tts`、`mentions` |
| mentions | **硬編碼 AllowedMentions = none**（不開放設定；@everyone/@role/@user 一律不推送，`<@id>` 只顯文字）。要 @ 人 → 以後再說 |
| Group DM | **放棄**（查證：bot 無法加入 Group DM type 3、收不到事件）。多人小圈圈不支援 |
| Room 抽象 | 因「放棄」而**降級**：不再有 Room 型別，改為 routing rule 上的 `alias`（agent 看到的是 alias，底下就是 channel ID） |
| 多 agent | 終局走 **HTTP + per-agent token**（`/mcp/<agent>` + Bearer）；unix socket 視情況（1:1 免 token）。同 token 只能一條 gateway ⇒ 單 process 多 agent。MVP 走 stdio 單 agent，但 `Principal{AgentID}` 從 M0 就在 ctx 裡 |
| 介面契約 | **LLM 只送文字**（path / url / ID / 內文）；**二進位全部 server 端處理**：出口由 server 從 `--file-root` 開檔上傳、入口由 server inline 或落地回 path。無 `base64` 入參 |
| 附件 | **config 驅動三段式**：`inline` / `save`(落 fs 回 path) / `skip`(不下載只列 URL+meta)。**定案預設：image=inline、audio=skip、video=skip、other=skip** |
| 未匹配路由的 DM | **定案：ignore**（完全不進 ring，也不建 target）；保留 `--unknown-dm` 以便日後改 policy |
| 環境檔 | 支援 **`.env`**（自己寫 10 行 loader，不引外部依赖；`--env-file` 預設 cwd 的 `.env`，存在才載） |
| persona | 不做 |
| 本機檔讀取 | `os.OpenRoot` / `*os.Root`（RESOLVE_BENEATH，擋 `..`/absolute/symlink 逃逸） |
| RequestTimeout | 保持 0；若被設定自動抬到 `max_wait+jitter+30s` |
| mcptransport | MVP 零修改（除了一個 4 MiB 常數的取捨，見 C2） |

---

## 0. 現況
已有 `mcp-discord-roleplay/go.mod`（go 1.27、`replace mcptransport => ./mcptransport`）與完成度高的 `mcptransport/`（stdio + Streamable HTTP + Registry + ContentPart + coerce，含測試）。**無** main.go / Discord 碼 / go.sum / README。
- stdio 每 request 一條 goroutine → 阻塞式 poll 不卡別人 ✅；`notifications/cancelled` 會 cancel 該 request 的 ctx → poll 必須 select `ctx.Done()` ✅
- HTTP POST-only（2026-07-28 拿掉 GET/SSE）→ 無法主動推播 ⇒ **long-poll 是正解**
- `Schema` 無 minimum/maximum/default/anyOf → 範圍與「三選一」在 handler 驗證並寫進 description
- `ToolResult` 無 structuredContent → 一律回 compact JSON 文字（+ image/audio content parts）
- stdio 單行 / HTTP body 上限 **4 MiB**（`stdio.go:67`、`server.go:15`）→ 直接制約 inline 大小（見 C2）
- sandbox 無 shell/go：我寫碼＋自我審查，`go build/test` 由你執行貼回；記 `_state/build-notes.md`

---

## 1. 架構
```
LLM(自研 agent) ⇄ mcptransport(stdio｜未來 HTTP+token)
      │ tools/call（ctx = Principal{AgentID}）
      ▼
 internal/tools   poll / send / history / channels / status
      │                    │
      ▼                    ▼
 internal/router    internal/bus  ←── internal/discord
  alias⇄channelID    ring+seq 游標     gateway handler → Event 正規化
  ACL(收/發)         broadcast         REST 發送 / history / 附件下載
  unknown 政策        filter            附件落 fs(save 模式)
                                  └──► discordgo.Session（全進程唯一 gateway）
```
```
mcp-discord-roleplay/
├── go.mod                  # + require github.com/bwmarrin/discordgo v0.29.0
├── main.go                 # 裝配；--transport stdio|http；--selftest；graceful shutdown
├── internal/
│   ├── config/             # Config + 驗證 + redact + routing.json
│   ├── router/             # Rule/Alias/Principal/ACL/unknown-dm 政策
│   ├── bus/                # Event Ring 游標 broadcast Filter
│   ├── discord/            # client.go events.go send.go target.go media.go
│   ├── media/              # 三段式附件處理：inline / save / skip；縮放；dedupe；os.Root 寫入
│   ├── files/              # 出口附件：os.Root 讀本機 / url 下載(SSRF 擋) / base64
│   └── tools/              # poll.go send.go history.go channels.go status.go
├── docs/DESIGN.md          # 本檔定稿後搬入
└── README.md
```

---

## 2. bus
```go
type Kind string // MVP: "message"、"system"(connected/reconnecting/disconnected)
type Event struct {
    Seq int64; Kind Kind; Ts time.Time
    Target string        // routing alias（agent 認得的「地方」）；未匹配時為 ""
    GuildID, ChannelID, ChannelName string; DM bool
    MessageID string; Author Author{ID,Name,Tag string; Bot,Self bool}
    Text string; MentionsSelf bool
    Attachments []Attachment{ID,Name,MIME string; Size int64; Width,Height int; URL,ProxyURL string; Local string}
    Embeds []EmbedSummary; Extra string
}
```
- Ring 容量 2000（`--bus-capacity`），覆蓋最舊並計數 → `dropped`
- 廣播用 `close(notify); notify = make(chan struct{})`（`sync.Cond` 無法配合 ctx/timer select）
- gateway handler 內只做正規化＋**非阻塞** append，絕不在 handler 呼叫 REST
- 游標 `"s<seq>"`；`cursor_key` 預設 = Principal.AgentID → 多 agent 游標天然隔離；poll 回傳時自動 ack（`no_ack` 除外）；寫 `--state-file`（重啟不重播）
- **啟動基線**：無游標 ⇒ 游標 = 當下 max seq（不回放舊訊），回 `reason:"baseline"`；要舊訊用 `backfill`/`history`
- Filter：`targets/channels`、`from`、`mentions_only`、`ignore_bots`(預設 true)、**`include_self`(預設 false)** ← 防回音循環
- Intents（MVP）：`IntentsGuilds|IntentsGuildMessages|IntentsDirectMessages|IntentsMessageContent`；Handlers：`MessageCreate/Ready/Disconnected/Resumed`

---

## 3. `discord_poll`
**語義（v2 修正）**：進場先查一次游標後面的既有緩衝 → 有就**立即**回；沒有就掛住，**任何一則符合條件的新訊息抵達的瞬間立刻回傳**（不累積、不等安靜期），最長等 `max_wait(±jitter)` 秒。
（同一瞬間連發的多則，因為是「一次讀光游標後全部」，自然會包在同一包裡回。）

```json
{"type":"object","properties":{
 "max_wait":{"type":"integer","description":"等待秒數，合法範圍 5..600，預設 45。期間一有新訊息就立刻回傳；超過時間則回傳空結果(reason=timeout)。實際值會加 ±15% 隨機抖動"},
 "targets":{"type":"array","items":{"type":"string"},"description":"只收這些路由 alias；省略=本 agent 全部可收的 target"},
 "channels":{"type":"array","items":{"type":"string"},"description":"進階：直接指定頻道/DM/使用者 ID（仍需過 ACL）"},
 "limit":{"type":"integer","description":"單次回傳事件上限，預設 20，hard cap 100（其餘留著下次讀，計入 dropped/unread）"},
 "mentions_only":{"type":"boolean","description":"true=只回 @我 或直接 DM"},
 "from":{"type":"array","items":{"type":"string"},"description":"作者過濾：user ID 或名稱"},
 "include_self":{"type":"boolean","description":"預設 false（true 會看到自己發的，小心回音循環）"},
 "ignore_bots":{"type":"boolean","description":"預設 true"},
 "media":{"type":"string","description":"本包附件處理覆蓋：inline | save | skip；省略=依 config（預設只有圖片 inline）"},
 "typing":{"type":"boolean","description":"等待期間在最近一個 target 顯示『正在輸入…』，預設 false"},
 "backfill":{"type":"integer","description":"僅在『尚無游標』時生效：向後抓 0..50 則，預設 0"},
 "no_ack":{"type":"boolean","description":"true=不推進游標（除錯用，會重複讀同一批）"}
}}
```
`max_wait` 校驗：`<5` 或 `>600` → `isError`：`{"ok":false,"error":"bad_max_wait","valid_range":[5,600],"default":45,"hint":"要立刻拿既有緩衝請用 discord_history"}`（**不提供 `max_wait=0`**；非阻塞需求由 `discord_history` 走 REST 承擔 — 見 C1）

偽碼：
```
poll(args):
  f := filter(args, principal)
  lo,hi := cfg.pollMin, cfg.pollMax                     // 5, 600
  if !between(args.max_wait, lo, hi) → isError(bad_max_wait)
  w := args.max_wait * (1 + rand(-jitterPct, +jitterPct))   // 預設 ±15%
  deadline := now + w
  cur := args.since or cursor.get(key) or baseline()
  evs := bus.After(cur, f, limit)
  if empty:
      select:
        <-notify    : evs = bus.After(cur,f,limit)      // 醒來看一眼就回（不湊數）
        <-timer     : return {reason:"timeout", waited_ms, cursor:cur}
        <-ctx.Done(): return {reason:"cancelled"}        // 不 ack
        <-shutdown  : return {reason:"shutdown"}         // 不 ack
  ack(evs.last.Seq) unless no_ack
  return envelope(evs)     // media 依 §5 處理
```
envelope：
```json
{"ok":true,"reason":"messages","waited_ms":3842,"max_wait_applied":44.1,
 "cursor":"s1042","next_cursor":"s1063","count":3,"dropped":0,"unread":7,
 "self":{"id":"...","name":"..."},"connected":true,"agent":"default",
 "messages":[{"seq":"s1043","id":"1234","target":"boss","dm":false,"guild":"222","channel":"111",
   "from":{"id":"333","name":"阿明","tag":"ahming","bot":false},
   "ts":"2026-11-16T19:20:31+08:00","age_s":12,"text":"晚安","truncated":false,"mentions_self":true,
   "attachments":[{"id":"a1","name":"night.png","mime":"image/png","size":182341,"width":940,"height":924,
      "url":"https://cdn.discordapp.com/...","media":"inline","content_index":1}]}]}
```
- 空結果要極短：`{"ok":true,"reason":"timeout","waited_ms":44213,"count":0,"cursor":"s1063","connected":true}`
- `reason` ∈ `messages|timeout|cancelled|baseline|shutdown|error`；**timeout 是 ok:true**
- inline 時 content = [text(JSON), image|audio...]，JSON 內用 `media:"inline"` + `content_index` 對照；`media:"skipped:too_big"` 等降級原因要寫明

---

## 4. `discord_send`
```json
{"type":"object","required":["to"],
 "properties":{
  "to":{"type":"string","description":"目標：alias（路由表）/ channel:<ID> / user:<ID> / #名稱 / @名稱 / me(最近 DM 對象) / last(最近互動的 target)"},
  "text":{"type":"string","description":"內文（單則 2000 字，超過自動依換行切多則）"},
  "files":{"type":"array","items":{"type":"object","properties":{
    "name":{"type":"string","description":"檔名；省略由 path/url 推"},
    "path":{"type":"string","description":"相對 --file-root 的本機路徑（os.Root 沙箱）。server 自己開檔上傳，LLM 不經手二進位"},
    "url":{"type":"string","description":"遠端 URL（server 下載後上傳；擋 private/link-local IP）"},
    "mime":{"type":"string","description":"省略則副檔名 + sniff"}}}}}
 }}
```
> **介面契約（定案）**：LLM 只送**文字**——路徑、URL、ID、內文。**二進位一律由 server 端處理**：出口從 `--file-root` 開檔上傳，入口用 `inline`/`save` 落地後回 path。沒有 `base64` 入參，也不期待它出現。
- `text` 與 `files` 至少其一；`to` 必須通過 **發送 ACL**（不屬於本 agent 的 target → `isError` + 提示）
- 附件載入 → `[]*discordgo.File{Name, ContentType, Reader}`；`MessageSend{Content, Files, AllowedMentions: none(硬編碼)}`
- >2000 字 → 依 `\n` 打包 2..N 則，附件掛最後一則
- `ChannelMessageSendComplex(ch, msg, discordgo.WithContext(ctx))`
- 回：`{"ok":true,"sent":[{"id":"1234","target":"boss","channel":"111","link":"https://discord.com/channels/@me/111/1234","chars":42,"attachments":1}],"note":"split into 2"}`
- 錯：`isError` + `{"ok":false,"error":"missing_access","hint":"bot 不在該伺服器或沒有 View Channel / Send Messages","detail":...}`（403/404/429 一律翻成可操作建議）
- 本機檔：`os.OpenRoot`，只收相對路徑、Clean 後 `root.Stat/Open`；上限 `--max-upload-bytes`(8 MiB/檔)、`--max-files-per-msg`(10)；`--no-file-root` 全關
- 提醒（未來跨機）：`path` 指 **server 那台**的檔案；agent 在別台就下 `url`（或未來把 server 搬到 agent 本機）

---

## 5. 附件處理三段式（v2 新增，config 驅動）
每種 media class（`image` / `audio` / `video` / `other`）各選一種 mode：

| mode | 行為 | 回給 LLM |
|---|---|---|
| `inline` | 下載 → （必要時縮放）→ base64 進 MCP result | 對 attachment 標 `media:"inline"` + `content_index`，另附 `url` |
| `save` | 下載寫入 `--media-out` 目錄（用 `os.Root` 沙箱；檔名 `<attachmentID>_<sanitizedName>`；已存在就跳過下載） | `media:"saved"` + `local_path`（給 agent 自己的工具讀/送 STT）+ `url` |
| `skip` | **不下載** | `media:"skip"` + `url`/`proxy_url`/size/mime（要圖再自己抓） |

**預設（定案）**：`image=inline`、`audio=skip`、`video=skip`、`other=skip`。（音訊依 C3 決定 skip，需要 STT 時你自己改 mode 即可）
**降級規則**：`inline` 超過 §6 上限 → 自動退 `save`（若 `--media-out` 有開）→ 再退 `skip`，並在 result 標 `media:"skipped:too_big"`（讓模型知道有東西但太大）。
**dedupe**：`state.json` 記 attachment id → 已 inline/save 過的再次出現只給 URL（防 `no_ack`/重試重複燒 token）。
`save` 的清理：`--media-keep` 天數 + `--media-max-mb` LRU（避免吃滿磁碟）。

---

## 6. 4 MiB 的真相（C2 改正）
**我 v2 的說法錯了，你對。** 重讀程式碼：
- `stdio.go:66-67` `scanner.Buffer(64KiB, 4<<20)` 是 **bufio.Scanner 讀 stdin（入口）**的單行上限
- `server.go:15` `maxBodyBytes = 4<<20` 用在 `server.go:77` 的 `io.ReadAll(io.LimitReader(r.Body, ...))` → 也是 **HTTP request body（入口）**
- **出口完全沒有上限**：`writeRPCStatus`(server.go:337) 與 `writeMessage`(stdio.go:263) 都是 `json.NewEncoder(...)` 直接串流到 writer，ContentPart 走 `MarshalJSONTo` 零中-copy，`bufio.Writer` 邊寫邊 flush
⇒ **tool result 內聯多大都不受 mcptransport 限制，4 MiB inline 可行，不必改套件、不必 vendor。**

真正的限制來自別處（所以要保留策略性上限，但那是**成本**問題不是**框架**問題）：
1. **模型 context**：raw 4 MB → base64 ≈ 5.3 M 字元 ≈ 130 萬+ token。除非你的 agent 把 base64 收起來自己轉 image block（我假設是），否則再寬的 transport 也沒用。
2. **各家模型 API 的圖片上限**：Anthropic base64 圖 ≤5 MB、OpenAI ≤20 MB、Gemini ≤20 MB/request。→ 4 MB 內聯在「圖」這個尺寸剛好安全。
3. **你自己 client 的讀取上限**：stdio 是 newline-delimited JSON，你 client 端讀一行もし設了 buffer 上限，就要跟我們對齊（我會把每包實際 bytes 寫進 `result_bytes` 方便你 debug）。

**入口/出口的分工（因為 LLM 永不送二進位，問題變簡單了）**：
- **入口（client → server）**只有文字：target/alias、ID、路徑、內文 → 單行離 4 MiB 遠得很。僅保留 3 行防呆：`main.go` 檢查 `Serve()` 回傳是否 `bufio.ErrTooLong` → 明確 log + 非零 exit（避免任何超大入參把進程「假死」式收掉）。**不需要 `--max-arg-base64-bytes` 這種東西，已從設定表移除。**
- **出口（server → LLM）**是所有二進位的路徑（`--file-root` 開檔 inline），而出口**無上限** → 尺寸只由下面的成本政策管。
- 對稱性：進（`save` 回 `local_path`）出（`files[].path` 讀 `--file-root`）**都以檔案路徑為介面**，LLM 世界裡沒有 base64。

**inline 尺寸政策（定案值，純成本考量，全部可調）**：
`--inline-max-bytes-image 4194304`(4 MiB)、`--inline-max-side 1280`（png/jpeg 本地 `x/image/draw` 縮；或先試 Discord media proxy `?width=1280` 省 CPU，實测定案）、`--inline-max-count 4`/包、`--inline-max-total-bytes 12 MiB`/包、超限走降級鏈（inline→save→skip）。

---

## 7. 其他工具
- MVP：`discord_status`（身分/gateway/重連/最近錯誤/ring 用量/各 cursor/路由與 ACL 摘要/版本）、`discord_channels`（可見 guild/頻道/DM + alias⇄ID 對照；模型靠它拿到能貼的 ID）、`discord_history`（REST ≤100 則、before/after，輸出格式同 poll 的 message；也是「立刻拿既有內容」的替代路徑）
- P2：`typing`、`react`、`edit`、`delete`、`poll_ack`、`ignore`、threads、progress 心跳（多 agent 遠端時才有意義）
- 註：bot 無「標已讀」API（只有 thread read-state）→ 不做

---

## 8. 設定
優先級 flag > env > `--config routing.json` > 預設
| 鍵 | 預設 |
|---|---|
| `DISCORD_TOKEN`（`--token-env` 指定變數名）| 必填；**只讀 env/file**，log redact，自動加 `"Bot "` |
| `--agent` | `default` |
| `--config` | routing.json（見下） |
| `--transport` | `stdio`（`--http-addr` 127.0.0.1:8787、`--origin`、未來 `--auth` per-agent token） |
| `--poll-min-wait / --poll-max-wait / --poll-default-wait / --poll-jitter-pct` | 5 / 600 / 45 / 15 |
| `--bus-capacity` | 2000 |
| `--state-file` | `mcpdr-state.json`（**cwd 底下**，空字串停用）|
| `--media-image / --media-audio / --media-video / --media-other` | `inline / skip / skip / skip` |
| `--media-out` | `mcpdr-media`（**cwd 底下**）；`--no-media-out` 停用 save |
| `--media-keep / --media-max-mb` | 7 天 / 512 MiB |
| inline 尺寸政策 | 見 §6（`--inline-max-bytes-image 4MiB` 等）|
| `--max-arg-base64-bytes` | **不需要**（LLM 不送 base64；見 §6 入口/出口分工）|
| `--env-file` | `.env`（存在才載；自己寫 loader，不引依赖）|
| `--allow-guilds / --allow-channels / --deny-channels / --allow-dm` | 空=不限制 / true（沒 routing.json 時的簡化 allowlist，建議一律填） |
| `--unknown-dm` | `ignore`（定案；不進 ring） |
| `--max-sends-per-min / --send-gap-ms / --breaker-errors` | 20 / 250 / 3（斷路 60s） |
| `--file-root`（可重複）/ `--no-file-root` | cwd / off |
| `--max-upload-bytes / --max-files-per-msg` | 8 MiB / 10 |
| `--dry-run` | false |
| `--log-level / --log-file` | info / stderr（slog；**stdout 只能是 MCP 訊息**） |

 **路徑政策**（user 定案）：所有路徑參數（`--state-file` / `--media-out` / `--log-file` / `--env-file` / `--config` / `--file-root`）一律**照原樣使用**——絕對路徑，或相對 process cwd。我們**不解析 `~`**（那是 shell 的事）、不查 `$HOME` / `$XDG_CACHE_HOME` / `$TMPDIR`、不把值改写到別的地方；預設值也都是純檔名，所以沒設定就落在 cwd。唯一例外：`--file-root` 內部走 `filepath.Abs`，因為 chroot containment 要比對一致的目錄字串（這也正是「相對 = cwd」的實作）。

routing.json（未來多 agent 直接長在這裡）：
```json
{"agents":[{"id":"a1","targets":["boss","dm:333"]}],
 "targets":[
   {"alias":"boss","id":"channel:111222333","agent":"a1","read":true,"write":true,"name":"#rp-main"},
   {"alias":"dm:333","id":"user:333","agent":"a1"}
 ],
 "unknown_dm":"ignore"}
```

安全：token 不落地 argv；發送 ACL；`os.Root` 沙箱（讀與 save 寫）；URL 下載解析後驗 IP（擋 private/link-local/localhost）；限流+斷路器；intents 最小化；README 寫「沒開 Message Content intent → content 為空字串」這個最常見的坑。

---

## 9. discordgo 要點（v0.29.0 核對過）
`discordgo.New("Bot "+token)`（內建 State/Ratelimiter、自動重連、自動 retry 429）；`s.Intents = IntentsGuilds|IntentsGuildMessages|IntentsDirectMessages|IntentsMessageContent`；`AddHandler(MessageCreate/Ready/Disconnected/Resumed)`；`Open()/Close()`；
`ChannelMessageSendComplex(ch,*MessageSend,...RequestOption)`、`MessageSend{Content,Embeds,TTS,Components,Files []*File,AllowedMentions,Reference,StickerIDs,Flags,Poll}`、`File{Name,ContentType,Reader}`、`ChannelMessages(ch,limit,before,after,around)`、`UserChannelCreate(recipientID)`、`ChannelTyping(ch)`、`ChannelMessageEdit/Delete`、thread 群（P2）、`UpdateStatusComplex`。
**所有 REST 都吃 `WithContext(ctx)`**。型別以 v0.29.0 為準：`*MessageAttachment`（非舊名 `Attachment`）、`Embeds []*MessageEmbed`、`ChannelTypeGroupDM=3`（bot 收不到）。
⚠️ 同 token 多 process 會撞 gateway session → 多 agent 必須共用一條 gateway（單 process）。

## 10. mcptransport
MVP 零修改（除非選 §6 的 (B)，改兩行常數 + 一個 config）。P2 才考慮：progress 心跳、`Schema` 補 min/max/default、`ToolResult` 補 structuredContent。

---

## 11. 里程碑
| | 內容 | 驗收（你跑） |
|---|---|---|
| M0 | go.mod + config + router 骨架 + discord/client（狀態機）+ main（`--selftest`、shutdown：在飛 poll 全部 `reason:"shutdown"` 再 Close）+ `discord_status` | `go mod tidy && go build ./...`；`go run . --selftest` 印身分/guild/target 表 |
| M1 | target 解析 + files(os.Root/url/base64) + 限流/斷路 + `discord_send` | 對測試 DM/頻道：純文字、純圖、文+圖；錯誤路徑 hint 可讀 |
| M2 | bus + MessageCreate 正規化 + `discord_poll`（下限/jitter/cursor/持久化）+ media 三段式 | 發訊→瞬間回；不發→timeout；Esc→cancelled；重啟不重播；大圖降級正確 |
| M3 | `discord_history`、`discord_channels` | alias⇄ID 可用、history 補舊訊 |
| M4 | README（Portal/intents/routing/接入）、拔網線 30s 重連不漏不炸、多 agent 檔預演 | e2e |

## 12. 測試計畫（不需要 token 的部分都寫成單測）
bus（游標/ring 覆蓋 dropped/broadcast 不丟/ctx cancel/jitter 區間/filter 組合）、router（規則優先序、ACL 拒發、unknown-dm）、target（`user:`/`channel:`/alias/`#名`/`@名`/`me`/`last`；模糊名回候選不瞎猜）、files（os.Root 逃逸 absolute/`..`/symlink 全擋、mime sniff、大小數量上限、url private IP 用 httptest 擋）、media（三 mode × 降級鏈 × dedupe，用極小測試圖/音檔）、send（抽象 `api` interface + fake session 驗 split 打包與 AllowedMentions=none）、poll（假事件序列 → envelope 斷言）、`--selftest`（真連煙霧測試）

---

## 13. 決策全數拍定
- **C1** poll 維持 v2 設計（下限 5s、無 `max_wait=0`；要立刻拿既有內容走 `discord_history`）✅
- **C2** 我搞錯了：4 MiB 只在入口，出口 streaming 無上限 ⇒ **不改 mcptransport、不做 vendor**。而且 **LLM 永不送二進位**（只送 path/url/ID/文字），二進位全由 server 從 `--file-root` 處理 → 入口容量議題不存在，只剩出口尺寸政策。`files` 入參**拿掉 `base64`**
- **C3** 音訊 `skip` ✅（video/other 亦 skip）
- **C4** 未匹配 DM `ignore` ✅
- 工具鏈：`go.mod` 樣板照舊（go 1.27 + replace mcptransport），正規寫法，不 vendor 不 proxy；有問題再處理
- 頻道/使用者 ID：user 自己處理，我產出 routing.json 範例格式即可

## 14. 動工順序（M0 交付物）
1. `go.mod` 加 `require github.com/bwmarrin/discordgo v0.29.0`（正規 `go mod tidy`）
2. `internal/config`：全部鍵 + `.env` loader + 驗證 + redact + routing.json 解析
3. `internal/router`：alias⇄target、Principal、ACL、unknown-dm=ignore
4. `internal/discord/client.go`：Session 生命週期、Ready/Disconnected/Resumed 狀態機
5. `main.go`：`--selftest`、`--transport`、SIGINT/EOF → 在飛 poll 回 `reason:"shutdown"` → `Session.Close()`
6. `internal/tools/status.go`：`discord_status`
7. 交給你跑：`go mod tidy && go build ./... && go run . --selftest`，輸出貼回 → 我修 → 記錄在 `_state/build-notes.md`
（M1 起才碰 send/poll/media；`files` 的 base64 上限與 ErrTooLong 對策在 M1 內完成）
