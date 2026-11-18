# mcp-discord-roleplay — 設計 **v3（定稿候選）**

- 2026-11-16　v0→v1→v2→**v3**。v3：C1/C3/C4 拍定；**C2 我原先的說法是錯的，已改正**（4 MiB 是「入口」限制，出口 streaming 無上限）
- 狀態：**設計定案（v3.1）＋ M2b 已實作、輸出已極簡化，`go vet` / `go test` 全綠並已接入 live**。本檔是實作的依據。
- 實作進度：**M0 ✅**（config / router / bus / discord gateway client / `discord_status` / `--selftest`）、**M1 ✅**（`internal/files` 沙箱 + `discord_send` + 預算/間隔/斷路器 + 單測）、**M2a ✅**（per-agent Queue/Hub + `router.Audience` + `discord_poll`）、**M2b ✅**（media 三段式 inline/save/skip）、**M2c 🚧**（target 語法收斂：`dm:` 單一字首、`dm:*`/`channel:*` wildcard、per-agent `read`/`write` 預設可收可放、閘門剩 `--allow-channels`/`--deny-channels`/`--unknown-channel`，`--allow-dm`/`--unknown-dm` 刪除；程式碼與測試已落地，待 `go build/test` 驗證）、**輸出極簡化 ✅**（2026-11-18：LLM 只拿必要欄位，其餘進 debug log；見 §3 的 wire 結構與判定三閘）。第一輪 live smoke 已跑（紅項只有入站圖片的 ctx timeout，已修），M3 = history/channels，M4 = README。
- ⚠ **`-race` 仍未跑過**（`go build` / `go vet` / `go test` 已由 user 驗證全綠）。驗證紀錄 → `_state/build-notes.md`。
- 建置驗證由 user 執行（sandbox 內無 shell / 無 go）：見 `_state/build-notes.md`。
- 環境：**Go 1.27.4**（已確認）、單一 bot token、client = 自研 agent、測試由 user 直接把 token/指令接進 session
- 依賴：`github.com/bwmarrin/discordgo v0.29.0`（API 簽名已逐條對過原始碼）

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
| 未匹配路由的頻道（**guild 與 DM 同一套**，M2c） | **定案：ignore**（完全不進 ring，也不建 target）；`--unknown-channel=ring` 才收，且只進預設 agent、以 raw channel id 登記 |
| Target 語法（M2c） | **只剩 `channel:<ID>` 與 `dm:<ID>`**；`dm:` 的 ID 是**對方 user id**（不是 DM 頻道 id）。Wildcard：`dm:*`＝所有 DM、`channel:*`＝所有 guild 頻道（**不含** DM ⇒ 兩條不重疊）。`user:` 前綴與 `--allow-dm`／`--unknown-dm` 整個刪除（不留 deprecation）：DM 就是一條頻道，柵欄也共用同一套 |
| 環境檔 | 支援 **`.env`**（自己寫 10 行 loader，不引外部依賴；`--env-file` 預設 cwd 的 `.env`，存在才載） |
| persona | 不做 |
| 本機檔讀取 | `os.OpenRoot` / `*os.Root` 當沙箱根；只用 `/` 當分隔符，開頭 `/` 等於根（`/x.png` == `x.png`）；剩餘照 `filepath.IsLocal`（`..`/`~`/volume name 拒），多 `--file-root` 依宣告順序第一個命中（不做 mount 語法） |
| 出站 `url` | **只貼不抓**：驗證後原樣附加到內文，由 Discord 自己拉（零 SSRF 面、不建 embed） |
| 429 處理 | 關掉 discordgo 自己的重試（`ShouldRetryOnRateLimit=false`、`MaxRestRetries=2`）；**直接回 `rate_limited`/`circuit_open` + `retry_after_ms`**，斷路器期間 server 端自動擋掉後續呼叫（不碰 REST） |
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
  alias⇄channelID    per-agent Queue   gateway handler → Event 正規化
  ACL(收/發)         Audience fan-out  REST 發送 / history / 附件下載
  unknown 政策        Drain＝取走即 ack  附件落 fs(save 模式)
                                  └──► discordgo.Session（全進程唯一 gateway）
```
```
mcp-discord-roleplay/
├── go.mod                  # + require github.com/bwmarrin/discordgo v0.29.0
├── main.go                 # 裝配；--transport stdio|http；--selftest；graceful shutdown
├── internal/
│   ├── config/             # Config + 驗證 + redact + routing.json
│   ├── router/             # Rule/Alias/Principal/ACL/unknown-channel 政策
│   ├── bus/                # Event + 每 agent 一條 Queue（ring）+ Hub 派發/統計
│   ├── discord/            # client.go events.go send.go target.go media.go
│   ├── media/              # 三段式附件處理：inline / save / skip；dedupe；os.Root 寫入（**不縮放**，超大就 skip）
│   ├── files/              # 出口附件：os.Root 讀本機上傳 / url 只貼不抓 / 無 base64 入參
│   └── tools/              # poll.go send.go history.go channels.go status.go
├── docs/DESIGN.md          # 本檔定稿後搬入
└── README.md
```

---

## 2. bus（M2a：per-agent queue，取走即消失）
```go
type Event struct {
    Kind string; Ts time.Time; Time string  // "message" | "system"；Time = Discord 原樣 ISO8601（帶時區偏移）
    Target string        // 內部座標 "channel:<id>"；normalize() 一律填 channel 座標，poll 時再換成 agent 認的 alias
    GuildID, ChannelID, ChannelName string; DM bool
    MessageID string; Author Author{ID,Name,Tag string; Bot,Self bool}
    Text string; Truncated bool; MentionsSelf bool
    Attachments []Attachment{ID,Filename,ContentType,Kind string; Size,Width,Height int; DurationSecs float64; URL,ProxyURL string}
    Embeds []Embed{Type,Title,Description,URL string}; Extra map[string]string   // "peer"=DM 對方 ID 等雜項
}
```
- **沒有 Seq、沒有游標**：取走就是 ack（LLM 端零狀態）。重啟丟 queue 是**設計上就決定這樣的**，補舊訊是 M3 `discord_history` 的工作
- 每個在 routing.json 聲明的 agent 一條 `Queue`（`--bus-capacity`，預設 2000/**agent**，環形覆蓋最舊並累計 `dropped`）；`Hub.Fanout(ev, agents)` 把**同一個指標**塞進多條 queue
- ⚠️ **共用指標紅線**：`Push` 之後 Event 唯讀。任何 per-queue 的處理結果（M2b 的 `Media/LocalPath/ContentIndex`）只能放在 `tools` 的輸出結構（`pollAttachment`）；放進 `bus.Attachment` 會讓第二條 queue 讀到別的 agent 的值（例如 local_path 指向別人的檔）。已照此拆開，用型別讓編譯器擋死
- 醒來機制 `close(notify); notify = make(chan struct{})`（`sync.Cond` 無法配合 ctx/timer select）；`Push` 非阻塞
- gateway handler 內只做正規化＋**非阻塞** append，絕不在 handler 呼叫 REST
- `Drain(want)` 在鎖內跑 filter（`want` 必須是純計算，不得 I/O）；被 filter 掉／超出 limit／超出 byte 預算的，用 `RequeueFront` 原序放回，不丟
- Intents（MVP）：`IntentsGuilds|IntentsGuildMessages|IntentsDirectMessages|IntentsMessageContent`（**Message Content 可用 `--no-message-content` 關掉**，見 §8）；Handlers：`MessageCreate/Ready/Disconnected/Resumed`

---

## 3. `discord_poll`（M2a 定稿）
**語義**：從**呼叫者自己的 queue** 一次取走最多 `limit` 則、且不超過 `--poll-max-bytes` 的訊息，**取走就從 queue 消失**（drain 語意 = ack，不需要游標）；queue 空就掛住，**任何一則符合條件的新訊息抵達的瞬間立刻回傳**（不累積、不等安靜期），最長等 `max_wait(+jitter)` 秒。
（同一瞬間連發的多則，因為是「一次讀光 queue 頭」，自然會包在同一包裡回。）

```json
{"type":"object","properties":{
 "max_wait":{"type":"number","description":"等待秒數，合法範圍 5..600，預設 45。期間一有新訊息就立刻回傳；超過時間則回傳空結果(reason=timeout)。實際值會加 +0..15% 隨機抖動（只往後，不會縮到 client timeout 以下）"},
 "targets":{"type":"array","items":{"type":"string"},"description":"只收這些路由 alias 或 channel:<ID> / dm:<ID>；省略=本 agent 全部可讀 target。命中 filter 但沒送出去的訊息會留在 queue 裡下次再取"},
 "channels":{"type":"array","items":{"type":"string"},"description":"進階：直接指定頻道/DM/使用者 ID（仍需過 ACL）"},
 "limit":{"type":"integer","description":"單次回傳事件上限，預設 20，hard cap 100（其餘留 queue 下次讀）"},
 "mentions_only":{"type":"boolean","description":"true=只回 @我 或直接 DM"},
 "from":{"type":"array","items":{"type":"string"},"description":"作者過濾：user ID 或名稱"},
 "include_self":{"type":"boolean","description":"預設 false（true 會看到自己發的，小心回音循環；會順帶放行 bot）"},
 "ignore_bots":{"type":"boolean","description":"預設 true"}
}}
```
（`mcptransport.Schema` 沒有 min/max/default 欄位 ⇒ 範圍與預設只能寫進 description，並由 handler 驗證。）

`max_wait` 校驗：`<5` 或 `>600` → `isError`：`{"ok":false,"error":"bad_max_wait","min":5,"max":600,"default":45,"hint":"..."}`（**不提供 `max_wait=0`**；補舊訊走 M3 的 `discord_history` — 見 C1）

偽碼：
```
poll(args):
  f := filter(args, principal)                        // 解析 targets 時就要求 Read 權限
  w := validate(args.max_wait, 5..600) else isError(bad_max_wait)
  deadline := now + w*(1 + rand(0..jitterPct))        // 只往後
  for {
    msgs, kept, remaining := q.Drain(want=f, limit, byteBudget)   // 鎖內取走
    if len(msgs)>0 || now >= deadline { break }
    select {
      <-q.Notify() : continue                          // 有新東西，重試 Drain
      <-timer      : continue                          // 到點，跑最後一次 Drain 後 break
      <-ctx.Done() : q.RequeueFront(kept); return {reason:"cancelled"}
      <-shutdown   : q.RequeueFront(kept); return {reason:"shutdown"}
    }
  }
  return envelope(msgs, remaining)     // 先回文字骨架
  // 附件在**第二相位**處理（見 §5）：預算裁完才抓，被丟掉的附件一個 request 都不花
```
poll 有**兩個結構**：工作結構 `pollMessage`／`pollAttachment`（欄位齊全、帶帳本、**從來不被 marshal**）與 wire 結構 `pollWire`／`msgWire`／`attWire`／`embedWire`（**唯一會被吐給模型的東西**）。好處：預算量的就是實際上線的位元組、`bus.Author`/`bus.Embed` 的形狀不再泄到線上、而**砍一個欄位 = 刪 wire 一行**，工作結構與 log 不受影響。

envelope（`omitempty` 的欄位只在有意義時出現；下面把常見的都列齊）：
```json
{"ok":true,"reason":"messages",
 "messages":[{"target":"boss","author":"阿明","author_id":"222333444555666777","ts":"2026-11-16T19:20:31+08:00","age_s":12,
   "text":"晚安","mentions_self":true,
   "attachments":[{"name":"night.png","kind":"image","url":"https://cdn.discordapp.com/...",
      "media":"base64","content_index":1}],
   "embeds":[{"title":"..."}]}],
 "remaining":7,"dropped":1,"notes":["..."]}
```
- **空結果恰好 2 個 key**：`{"ok":true,"reason":"timeout"}`（quiet server 是常態，而每一個 byte 都會進模型的 context）
- 預算算的是**上面這個 wire 結構**的 JSON，不是工作結構：`messageCost` 量 `m.wire()` ＋（圖 1000 / 其他附件 300 / embed 200 的估價）。inline 的 base64 在 content block 裡、**不在這段 JSON 裡** ⇒ 不佔預算（舊版那個 `result_bytes` 固定點迴圈就是為了湊一個實際上沒人在用的數字，已整段拿掉）
- `reason` ∈ `messages|timeout|disconnected|cancelled|shutdown`；**timeout 是 ok:true**。沒有訊息且 gateway 不在聽時，`timeout` 會被改寫成 `disconnected`（對一條死線路說「沒人說話」是誤導）；`cancelled`/`shutdown` 保留自己的字，它們講的是「這一問其實沒送到你手上」
- **byte 預算**（`--poll-max-bytes`，預設 16 KiB，0=不設限）：只算 messages 陣列的 JSON；每則成本 = 實際位元組 +（圖 1000 / 其他附件 300 / embed 200 的估價）。裝不下的整則留 queue。單條就超預算 → 先按 **rune 邊界**裁 `text`（標 `text_truncated:true`，原本的位元組數寫進 `notes`），再丟附件（少了幾個也寫進 `notes`），最後丟 embeds；估價**跟著留下的附件走**（丟掉的不再佔位，否則多圖訊息會一個都列不出來）；裁完還是超 → 不送空殼，整則留著
- `remaining`（**只在 >0 時出現**）＝ 佇列裡還有幾則 ⇒ **請立刻再 poll**；被 filter 掉而留下的不計在內，免得模型無限 poll。（舊版另有一個 `partial:true` 布林，語意跟這個數字重複，已刪；它進 log）
- `dropped` 只在 >0 時出現：那是「沒人 poll 把 queue 擠爆、被覆蓋掉的舊訊息」，要直接告訴模型
- `target` 標的是**這個 agent 認得的 alias**（`router.AliasForTarget` 在組裝時現算）；沒有 rule 收養它就留座標——DM 標 `dm:<peer>`（能直接回話的形式），其餘 `channel:<id>`。wildcard rule 的 alias（`dm:*` 的 `dms`）**不是地址**，所以也不拿去當前綴。同一個 Event 在兩條 queue 裡可以標不同名字，所以這個欄位只能長在 `pollMessage` 上
- 作者壓成一條字串 `author:"阿明"`，名字取的是 **Discord 自己會顯示的那個**：`Message.Member.Nick`（群內暱稱）優先，沒有才退回 `User.DisplayName()`（`global_name` → `username`）——只讀 `DisplayName()` 的話，操作者改了群內暱稱而 agent 永遠叫錯人。`bus.Author` 的 `tag`（舊式 discriminator）對回話沒有增量 ⇒ 進 log；空名字才退回 `tag`，不給匿名台詞。唯一的 ID 例外是 `author_id`（見下方判定三閘）：它過第②閘，因為 `discord_send` 的 `to` 吃 `dm:<ID>` ⇒ 沒有它，agent 無法回 DM 給「剛講話那個人」，只能用 routing 檔裡的 alias；`buildEvent` 的佔位 `"?` 不上線（回 `to:"dm:?"` 是個陷阱）。
- 「這條訊息本來有幾個附件、被預算擠掉幾個」不再是欄位：留下的都在 `attachments` 清單裡，少幾個由 `notes` 講。別跟 `dropped` 混：那是**整則訊息**被 queue 擠爆覆蓋掉
- **附件在輸出裡只有三種形態**（`policy` 不上線，只告訴模型你**拿到**了什麼）：`media:"base64"` + `content_index`（≥1：`content[0]` 是這段 JSON，圖 block 接在後面）／`media:"file"` + `local_path`（可直接餵 `discord_send` 的 `files[].path`）／`media:"url"` + `reason`（見 §5 詞表）。附件在線上只有 `name`（原 `filename`）、`kind`、`url`、`media`，加條件欄位 `duration_secs`（僅音訊／視訊，那是唯一的內容描述）／`reason`／`local_path`／`content_index`；`id`、`mime`、`size`、像素邊長、實際交付位元組數全進 log
- **上線欄位的判定三閘**（往後新增欄位一律先過這三閘）：①要用它回話 ②要據它決定下一步 ③是敘事的必要輸入。ID、時間戳、位元組數、像素尺寸、alias 背後的真實頻道名都是「誰在何時算了什麼」⇒ 那是 log 與 `discord_status` 的活（`discord_status` 就是這個面板本體，約 90 個 key，所以它是「偶爾呼叫」而不是每輪呼叫）。目前唯一的 ID 例外就是 `author_id`
- 被搬走的欄位沒有蒸發：`-log-level=debug` 時 poll 打一條 `poll`（`agent`、`waited_ms`、`max_wait`、`connected`、`self`、`count`、`remaining`、`partial`、`bytes`、`dropped`、`notes`、`result=`整個 wire），send 打一條 `send`（`agent`、`target`、`kind`、`channel_id`、`parts`、`message_ids`、`delivered`、`created_at`）。預設 log level 下兩者都被 `Enabled(Debug)` 守門 ⇒ 零成本

---

## 4. `discord_send`（M1 已實作）
```json
{"type":"object","required":["to"],
 "properties":{
  "to":{"type":"string","description":"目標：alias（路由表）/ channel:<ID> / dm:<ID> / #名稱 / @名稱 / me(傳给自己的 DM) / last(本 agent 最近說話的 target)"},
  "text":{"type":"string","description":"內文；超過 2000 字元自動切多則依序送出"},
  "files":{"type":"array","items":{"type":"object","properties":{
    "name":{"type":"string","description":"Discord 顯示的檔名；省略取 path 的 basename"},
    "path":{"type":"string","description":"相對某個 --file-root 根目錄的檔案路徑（os.Root 沙箱）。server 自己開檔上傳，LLM 不經手二進位"},
    "url":{"type":"string","description":"http(s) 連結：**不下載**，原樣貼進內文讓 Discord 自己抓"},
    "mime":{"type":"string","description":"省略則副檔名 → sniff 前 512 byte → application/octet-stream"}}}}}
 }}
```
> **介面契約（定案 v3.1）**：LLM 只送**文字**——路徑、URL、ID、內文。**二進位一律由 server 端處理**：出口從 `--file-root` 開檔上傳，入口用 `inline`/`save` 落地後回 path。沒有 `base64` 入參，也不期待它出現。

### 4.1 URL 是「貼上」不是「下載」（user 裁示）
「本來就不該我們抓下來轉送，只負責複製貼上 url 跟從 fs root 把檔案塞進去。」
- `files[].url` 驗證通過後，**逐條附加到內文結尾**（空行分隔、一行一條），不建 embed、不開任何出站連線。
- 好處：零 SSRF 面（我們不碰目標位址，沒有 private/link-local/redirect 的問題要處理）、零頻寬與暫存、Discord 自己渲染 preview。
- 驗證只問「這是一條乾淨的 http(s) URL 嗎」：scheme ∈ {http,https}、要有 host、長度 ≤2048、**不得含任何 <0x21 或 0x7f 的位元組**（換行會變成在訊息裡夾帶第二行＝語意注入）。回傳 `url.URL.String()` 的標準化結果。
- 注意：貼上的連結會算進 2000 字元的切段預算（先合體再切段）。

### 4.2 本機檔案沙箱（`internal/files`）
- **路徑政策照 user 裁示**：「`--file-root` 直接是 `/`，其他全照原本的 filepath 邏輯」——不做 `HOST=VIRT` 之類的 mount 語法；每個 `--file-root DIR` **本身就是根**。
- **分隔符只用 `/`（不分 OS）**：`--file-root` 們就是整個命名空間，所以開頭的 `/` 不攜帶資訊——`/pose.png` 與 `pose.png` 是同一個檔（user：「直接 `/` == `./` 就行」）。尾端的 `/` 也當飾邊處理；剝完只剩 `.` 的就是根本身 → `not_a_file`。
- **順序（user 定案：先 clean，再 `/` == `.`）**：`toRootPath()` = `filepath.ToSlash` → `path.Clean` → 剔掉開頭 `/`。用 `path.Clean` 不用 `filepath.Clean`：後者在 Windows 把 `\` 也當分隔符，跟「只用 `/`」相反。
- clean 先跑意味著 `".."` 在 root **內**只是別名：`./a/../b` 就是 `b`（允許）。剔完開頭 `/` 後仍剩 `..` 元素的，才是真的要離開 root → `path_escapes`；volume name（`C:/x`）同拒；`~/` 另校（IsLocal 覺得它合法，但我們永不展開）。另：`/../etc/passwd` 會被 clean 成 `etc/passwd`（命名空間的根之上沒有上層，跟 POSIX 絕對路徑一樣），所以是 `not_found` 不是 escapes。
- 然後對**每個 root 依宣告順序** `os.OpenRoot(root)` → `Open(name)`，**第一個命中的贏**，開完立刻 `rt.Close()`（Root 關掉不影響已開的 fd）。
- `os.Root` 會跟 symlink 但**禁止指向 root 外**（Go 端已處理，我們不重複實作）；它**不管 fs 邊界/bind mount/`/proc`/device file**，所以我們自己補 `Stat().Mode().IsRegular()`，且 `--file-root /` 會在 selftest/status 出現 WARN。
- 上限：`--max-upload-bytes`（預設 8 MiB/檔，訊息用 KiB/MiB 講清楚）、`--max-files-per-msg`（10）；`--no-file-root` 全部關掉 → `files_disabled`。**`*Store` 為 nil 時所有方法安全回 `files_disabled`**。
- 檔名過 `SafeName`（去目錄、控制字元一律換 `_`（不吞掉，留住邊界）、`*?"<>|:/` 與反斜線 → `_`、去前導點、≤120 rune 且保留副檔名、空 → `file.bin`）。
- 代碼：`files_disabled / empty_path / path_escapes / not_found / not_a_file / too_large / too_many_files / bad_url / open_failed`；`not_found` 的 tool 回覆會附 `file_roots` 讓 operator 看得出對不上。
- **先全數開好、再開始送**：壞路徑讓整次呼叫失敗，不會送出半個場景。

### 4.3 切段
- 常數 `discord.MaxContentRunes = 2000`（不是 flag：超過就是被拒，切段只有一個正確數字）。以 **rune** 計，不是 byte。
- `splitMessage` 順序：換行符 → 句末（`。．！？!?；;…` 並吸收隨後的閉符號 `”"’）)】]》」』`）→ 空白 → 硬切（並回報 `hardCut` 警告「可能切到單字中間」）。
- 候選切點必須 ≥ min（= limit/2），避免產出碎片；純 whitespace 段略過；每輪至少吃掉 min 個 rune（或硬切吃滿 limit），保證終止。
- 段數上限 `maxSendParts = 20`（=40k 字元），超過 → `text_too_long`，建議改寫成檔案當附件。
- **附件只掛在最後一段**；空 text + 附件是合法訊息（會 warning 說明）。
- 第 2 段起先查 `ctx.Err()`：呼叫者走了就停，並回報 `parts_sent`。

### 4.4 限流、預算、斷路器（`internal/discord`）
user 裁示：「直接回 429 rate limit 跟時間讓 LLM 知道，然後 mcp server side 自動擋掉後續時限內的 call。」

| 機制 | 行為 |
|---|---|
| 每分鐘預算 | `--max-sends-per-min`(20)：滑動 60s 視窗，**在 admitted 的那一刻就佔額度**（併發不會超賣）。耗盡且還要等 >5s → `budget_exhausted` + `retry_after_ms`；≤5s 我們自己等 |
| 同頻道間隔 | `--send-gap-ms`(250)：以頻道為鍵，不同頻道互不拖慢 |
| 本地吸收 | `--send-retry-wait-ms`(1500)：Discord 的 `retry_after` ≤ 此值 → 我們等一次再試（重試前 **`Rewind()` 附件**，否則第二次會上傳 0 byte）；較大 → 原封不動回報 |
| 斷路器 | 連續 `--breaker-errors`(3) 次 429 → 開啟 60s（`breakerHold`）：期間每個 send **完全不碰 REST**，直接 `circuit_open` + `retry_after_ms`；60s 後只放 **1 個 half-open probe**（其餘拿 `circuit_open`，`retry_after_ms=2000`）；probe 成功即重置，失敗再關 60s。非 429 的失敗也會歸還 probe 權，不會卡死 |

**為何關掉函式庫自己的重試**：`/_tmp/dg_v0290_restapi.go:279-298` — `ShouldRetryOnRateLimit = true` 時 429 **永遠不會變成 error**（`time.Sleep(rl.RetryAfter)` 後用同一 sequence 遞迴重試，且**不看 ctx**）。那樣一來斷路器是裝飾品、tool call 可能無限阻塞。所以 `Start()` 設 `ShouldRetryOnRateLimit = false`（429 以 `*RateLimitError` 回到我們手上）＋ `MaxRestRetries = 2`（只管 502）。

### 4.5 回覆格式（實作）
成功（**只有模型會據以行動的東西**，其餘進 log）：
```json
{"ok":true}
```
加條件欄位：`parts_sent`（**只在 >1**）、`attachments:[{"name":"pose.png","size":12345}]`（只在有掛檔）、`url_count`（只在 >0）、`dry_run`（只在 dry-run）、`warnings`（只在有話要講）⇒ 純文字一則的 send 就是 `{"ok":true}`，而 dry-run 是三個 key（`ok,dry_run,warnings`：那條 warning 正是「別以為真的貼出去了」的依據，所以它上線）。
失敗（`isError:true`）：`{"ok":false,"error":"<code>","hint":"<下一步>","retry_after_ms":...,"http_status":...,"discord_code":"...","breaker_open":true,"target":"boss","kind":"channel","channel_id":"...","parts_sent":1,"message_ids":[...],"part_failed":2,"delivered_before_failure":1}`
- 部分送達時 `hint` 內會寫「N of M part(s) landed … continue from part N+1」，**不要整則重傳**。
- **成功時話少是對的**：訊息已經在 Discord 上了，今天的工具沒有任何一個會 consume `message_ids`／`last_message_id`（將來 react/edit/delete 落地時它要回來，那時候它是「據以決定下一步」的欄位）。被搬進 log 的：`target`（LLM 自己填的）、`kind`、`channel_id`（alias 背後的真實 id）、`delivered`、`created_at`、`message_ids`。
- 代碼：`not_connected / circuit_open / budget_exhausted / rate_limited / cancelled / unauthorized / forbidden / unknown_channel / cannot_send_to_channel / bad_request / server_error / network_error / api_error / attachment_failed / dm_channel_failed`，加上路由層的 `empty_target / unknown_target / not_allowlisted / denied_channel / foreign_agent / write_denied / wildcard_target / no_last_target` 與檔案層的 9 個碼。
- `AllowedMentions{Parse: []}` 硬編碼（**空 slice 而非 nil**：`Parse` 不帶 `omitempty`，nil 會被整欄不送＝Discord 預設全解析 → @everyone 可被 ping）。無 flag 可關。
- 不回 `link`：訊息連結需要 guild id，我們不保證有；要對帳的話 `message_ids` 在 debug log 裡。
- DM target：`dm:<id>`（id 是**對方 user id**）走 `UserChannelCreate`（快取於 `Client.dm`，DM 頻道 id 穩定故不須失效）；`me` 用 `SelfID()`，尚未 READY → `not_connected`。
- `--dry-run`：不佔額度、不碰 REST、不建 DM；仍走完路由/沙箱/切段並在結果標 `dry_run:true`。


---

## 5. 附件處理三段式（v2 新增，config 驅動）
每種 media class（`image` / `audio` / `video` / `other`）各選一種 mode：

| mode | 行為 | 回給 LLM |
|---|---|---|
| `inline` | 下載 → （必要時縮放）→ base64 進 MCP result | 對 attachment 標 `media:"base64"` + `content_index`，另附 `url` |
| `save` | 下載寫入 `--media-out` 目錄（全程 `os.Root`；**檔名 `<8 hex>-<safe name>`**，token 由「每 process 亂數 secret ＋ attachment id ＋檔名」哈希而來 ⇒ 外部猜不到、同 process 可重現；**同名檔已存在就直接沿用，不再下載**） | `media:"file"` + `local_path`（=`--local-path` 前綴＋檔名，可直接回喂 `discord_send`）+ `url` |
| `skip` | **不下載**（零 I/O） | `media:"url"` + `reason` ＋ `url`（要圖再自己抓；`proxy_url`/size/mime 都只進 log） |

**預設（定案）**：`image=inline`、`audio=skip`、`video=skip`、`other=skip`。（音訊依 C3 決定 skip，需要 STT 時你自己改 mode 即可）
**超過上限怎麼辦（M2a 定案：不降級）**：`inline` 碰到任一上限 → 直接 `skip`，由 `reason` 講清楚是哪一條。不自動退 `save`：降級鏈會讓同一張圖在不同輪的形態飄移，模型無從判斷，而 save 也是要 I/O 的。
**上限共通規則**：`--inline-max-*` 與 `--media-save-max-bytes` 一律 **`-1`＝不設限、`0`＝該模式停用**，其他值＝上限。
**能力上限（不是降級）**：MCP 的 content block 只有 `text`/`image`/`audio`/`resource_link`（見 `mcptransport/content.go` 的 encoder），**沒有 video block** ⇒ `--media-video=inline` 得到 `media:"url"`＋`reason:"unsupported"`。
**`reason` 詞表**（只在 `media:"url"` 出現）：`policy`（設定就是 skip）、`unsupported`（該 kind 沒有 content block）、`too_big`（超過單檔 bytes／save 上限）、`side`（邊長超過 `--inline-max-side`，我們不縮放）、`no_budget`（超過每包 `--inline-max-count`／`--inline-max-total-bytes`）、`save_off`（設定要 save 但 `--media-out` 停用或目錄不可用）、`error`（抓檔／寫盤失敗；細節在 log，不上線）。
`save` 的清理：開機跑一次 `media.Cleanup`（先 `--media-keep` 天數、再 `--media-max-mb` LRU by mtime），只碰 regular file，子目錄一律跳過。
**timeout 的擁有權在讀端**（live smoke 抓出來的坑）：每檔 15s 由 `Session.fetchCtx()` 建立，`get`/`open` 只收 ctx、**自己不 `defer cancel()`**；cancel 跟著 `inline()`/`save()` 直到 body 讀完。原因是 net/http 把 response body 綁在發 request 那個 ctx 上，headers 一到就 cancel 會讓之後的 `Read()` 直接拿到 `context canceled`——本機 httptest 的 8 byte payload 連 header 同段進 buffer，所以單測摸不到，真 CDN 82 KB + TLS 第一個 chunk 就紅。

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
3. **你自己 client 的讀取上限**：stdio 是 newline-delimited JSON，你 client 端讀一行もし設了 buffer 上限，就要跟我們對齊（每包實際位元組現在只寫進 debug log 的 `poll` 行，不在回應裡了——那個 `result_bytes` 欄位已隨極簡化退場）。

**入口/出口的分工（因為 LLM 永不送二進位，問題變簡單了）**：
- **入口（client → server）**只有文字：target/alias、ID、路徑、內文 → 單行離 4 MiB 遠得很。僅保留 3 行防呆：`main.go` 檢查 `Serve()` 回傳是否 `bufio.ErrTooLong` → 明確 log + 非零 exit（避免任何超大入參把進程「假死」式收掉）。**不需要 `--max-arg-base64-bytes` 這種東西，已從設定表移除。**
- **出口（server → LLM）**是所有二進位的路徑（`--file-root` 開檔 inline），而出口**無上限** → 尺寸只由下面的成本政策管。
- 對稱性：進（`save` 回 `local_path`）出（`files[].path` 讀 `--file-root`）**都以檔案路徑為介面**，LLM 世界裡沒有 base64。

**inline 尺寸政策（定案值，純成本考量，全部可調）**：
`--inline-max-bytes-image 4194304`(4 MiB)、`--inline-max-side 1280`（**不縮放**：邊長超過就不內聯，我們不碰像素）、`--inline-max-count 4`/包、`--inline-max-total-bytes 12 MiB`/包、超限**不降級**，由 `reason` 說明是哪一條（見 §5）。

---

## 7. 其他工具
- MVP：`discord_status`（身分/gateway/重連/最近錯誤/**每 agent queue 深度與 dropped**/路由與 ACL 摘要/版本/下一步建議）、`discord_channels`（可見 guild/頻道/DM + alias⇄ID 對照；模型靠它拿到能貼的 ID）、`discord_history`（REST ≤100 則、before/after，輸出格式同 poll 的 message；也是「補舊訊」的唯一路徑）
- P2：`discord_typing`（**M2a 決定 poll 不做 typing**：掛住語意與它衝突）、`react`、`edit`、`delete`、threads、progress 心跳（多 agent 遠端時才有意義）
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
| `--bus-capacity` | 2000（**每個 agent 各一條**）|
| `--poll-max-bytes` | `16384`（16 KiB；0=不設上限。只算 messages 的 JSON，附件按 §3 估價計入）|
| `--media-image / --media-audio / --media-video / --media-other` | `inline / skip / skip / skip` |
| `--media-out` | `mcpdr-media`（**cwd 底下**）；`--no-media-out` 停用 save |
| `--local-path` | `""`＝由 `--media-out` 推。給 LLM 的 `local_path` 前綴：宣告「`--media-out` 這個目錄在 `--file-root` 眼裡叫什麼」，bind mount 也對得上 |
| `--media-save-max-bytes` | `4 MiB`（單檔落地上限；`-1` 不設限、`0` 停用 save。Discord 允許到 ~200 MB，超限不降級，直接 `media:"url"`＋`reason:"too_big"`）|
| `--media-keep / --media-max-mb` | 7 天 / 512 MiB |
| inline 尺寸政策 | 見 §6（`--inline-max-bytes-image 4MiB` 等）|
| `--max-arg-base64-bytes` | **不需要**（LLM 不送 base64；見 §6 入口/出口分工）|
| `--env-file` | `.env`（存在才載；自己寫 loader，不引依賴）|
| `--allow-guilds / --allow-channels / --deny-channels` | 空=不限制（沒 routing.json 時的簡化 allowlist，建議一律填）。名單元素**吃 alias 或 target id，先查 alias**；`channel:*` / `dm:*`＝這個 kind 全部 |
| `--unknown-channel` | `ignore`（定案；沒有 rule 收養、又不在 allowlist 的頻道——guild 與 DM 同一套——完全不進 ring；`ring` 給預設 agent）|
| `--max-sends-per-min / --send-gap-ms / --breaker-errors` | 20 / 250 / 3（斷路 60s） |
| `--file-root`（可重複）/ `--no-file-root` | cwd / off |
| `--max-upload-bytes / --max-files-per-msg` | 8 MiB / 10 |
| `--dry-run` | false |
| `--no-message-content` | false。關掉 **Message Content**（我們唯一的 privileged intent）：gateway 不再要求該 bit，incoming `text` 恆為空，但 `mentions` / `attachments` / `embeds` 不受影響照收（Portal 沒開卻硬要該 bit 會被 4014 踢）|
| `--log-level / --log-file` | info / stderr（slog；**stdout 只能是 MCP 訊息**） |

 **路徑政策**（user 定案）：所有路徑參數（`--media-out` / `--log-file` / `--env-file` / `--config` / `--file-root`）一律**照原樣使用**——絕對路徑，或相對 process cwd。我們**不解析 `~`**（那是 shell 的事）、不查 `$HOME` / `$XDG_CACHE_HOME` / `$TMPDIR`、不把值改寫到別的地方；預設值也都是純檔名，所以沒設定就落在 cwd。唯一例外：`--file-root` 內部走 `filepath.Abs`，因為 chroot containment 要比對一致的目錄字串（這也正是「相對 = cwd」的實作）。附件的 `path` 是另一回事，見 §4.2：在 root 之內，開頭 `/` 等於那個 root。

routing.json（未來多 agent 直接長在這裡）：
```json
{"agents":[{"id":"a1","targets":["boss",{"alias":"dms","write":true}]}],
 "targets":[
   {"alias":"boss","id":"channel:111222333","agent":"a1","read":true,"write":true,"name":"#rp-main"},
   {"alias":"dms","id":"dm:*","read":true,"write":false}
 ],
 "allow_channels":["boss","dm:888"],
 "deny_channels":["channel:999"],
 "unknown_channel":"ignore"}
```

分工（M2c 定案，三層不要混）：
- `agents[].targets`＝**誰收到**（per-agent fan-out）。元素是**字串 alias**（沿用 target 自己的預設）或**物件** `{alias,read,write}`＝這個 agent 的**覆寫**，**可收緊也可放寬**。
- target 上的 `read`/`write`＝**預設值**（不是上限）。理由：典型用法是「多數 agent 唯讀、只留一隻能寫」⇒ 預設方向必須 fail-closed。
- `allow_channels`/`deny_channels`＝**這條規則存不存在**（全域）；`deny` 是絕對上限（連讀都擋），`--deny-channels=dm:*` 就是過去 `--allow-dm=false` 的意思。
- **allow ∩ deny 同時命中** ⇒ 啟動 warning（`--selftest` 與 log 都看得到）＋ **deny 贏**。
- fan-out 去重：一個 event 同時被 exact rule 與 wildcard 命中時，**每個 agent 各自挑它持有最具體的那條** ⇒ 不會收到兩份，也不會因為別隻 agent 聲明了 exact 而漏接。
- wildcard 的 alias（例 `dms`）是**政策不是地址**：`discord_send` 拿它當 `to` 會回 `wildcard_target`；poll 也不會拿它當 `target` 標籤，改標 `dm:<peer>`。

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
| M1 | target 解析 + `internal/files`（os.Root 沙箱；url 只貼不抓；無 base64）+ 預算/同頻道間隔/斷路器 + `discord_send`（切段、附件掛末段）+ 三個 `_test.go` | `go build ./... && go vet ./... && go test ./...`；`go run . --selftest --file-root . --file-root ./docs`；`--dry-run` 下對測試頻道：純文字、純圖、文+圖、超長切段、壞路徑 hint 可讀、429/斷路器回 `retry_after_ms` |
| M2a | bus（per-agent Queue/Hub）+ router.Audience + MessageCreate 正規化 + `discord_poll`（下限/抖動/drain/byte 預算/裁切） | `go build ./... && go vet ./... && go test -race ./...`；發訊→瞬間回；不發→timeout；Esc→cancelled 且訊息還在；兩 agent 互不干擾 |
| M2b | media 三段式（inline/save/skip）接到 poll 輸出（**只寫 pollAttachment，不寫回 Event**） | 大圖降級正確；save 檔名去重；`local_path` 在 `--media-out` 內 |
| M3 | `discord_history`、`discord_channels` | alias⇄ID 可用、history 補舊訊 |
| M4 | README（Portal/intents/routing/接入）、拔網線 30s 重連不漏不炸、多 agent 檔預演 | e2e |

## 12. 測試計畫（不需要 token 的部分都寫成單測）
bus（ring 覆蓋 dropped/Notify 不漏/Drain-RequeueFront 原序/nil-Hub 防呆/唯讀契約）、audience（規則優先序、一 event 多 agent、stray、deny、unknown-channel）、router（規則優先序、ACL 拒發、unknown-channel、wildcard 退回、per-agent grant 覆寫）、target（`dm:`/`channel:`/alias/`#名`/`@名`/`me`/`last`；模糊名回候選不瞎猜）、files（路徑政策：先 `path.Clean` 再把開頭 `/` 當根（`./a/../b` == `b`）、`..` 倖存者/`~`/volume name 拒；symlink 逃出 root 被 `os.Root` 擋、根內可用；多 root 優先序；目錄/超大/數量上限；mime 四層優先序與 sniff 後 offset 歸零；`ValidateURL`）、send（seam 注入假 poster + 假時鐘：AllowedMentions.Parse 為空 slice、gap/預算/斷路器狀態機、小 `retry_after` 本地重試且**重試前有 Rewind**、 breaker open 時完全不碰 REST、half-open 只放 1、DryRun 不碰 REST 不佔額度、classify 全碼）、tools（`-dry-run` 端到端 envelope、各 `to` 拒因與 `targets` 候選、切段邊界 1999/2000/2001/CJK/附件僅末段、`text_too_long`、`last` per-agent 隔離、部分送達）、poll（假事件序列 → envelope 斷言、byte 預算截斷、裁切不切壞 UTF-8、queue 隔離、cancel 後訊息仍在、timeout 空答只有 2 個 key、**wire key 集合契約**（envelope/message/attachment/embed 四層各自釘死，新增欄位要先過 §3 三閘））、media（三段政策：inline 真的進 content block、save 一檔不重複下載、skip 零 request、超限不降級、**headers/body 分段交付也要讀得完**（E-8 回歸）、cleanup 年齡＋LRU）、`--selftest`（真連煙霧測試）

---

## 13. 決策全數拍定
- **C1** poll 維持 v2 設計（下限 5s、無 `max_wait=0`；要立刻拿既有內容走 `discord_history`）✅
- **C2** 我搞錯了：4 MiB 只在入口，出口 streaming 無上限 ⇒ **不改 mcptransport、不做 vendor**。而且 **LLM 永不送二進位**（只送 path/url/ID/文字），二進位全由 server 從 `--file-root` 處理 → 入口容量議題不存在，只剩出口尺寸政策。`files` 入參**拿掉 `base64`**
- **C3** 音訊 `skip` ✅（video/other 亦 skip）
- **C4** 未匹配頻道（guild 與 DM 同一套）`ignore` ✅（M2c 把 DM 的獨立政策併進來）
- 工具鏈：`go.mod` 樣板照舊（go 1.27 + replace mcptransport），正規寫法，不 vendor 不 proxy；有問題再處理
- 頻道/使用者 ID：user 自己處理，我產出 routing.json 範例格式即可

## 14. 動工順序（M0 交付物）
1. `go.mod` 加 `require github.com/bwmarrin/discordgo v0.29.0`（正規 `go mod tidy`）
2. `internal/config`：全部鍵 + `.env` loader + 驗證 + redact + routing.json 解析
3. `internal/router`：alias⇄target、Principal、ACL、unknown-channel=ignore
4. `internal/discord/client.go`：Session 生命週期、Ready/Disconnected/Resumed 狀態機
5. `main.go`：`--selftest`、`--transport`、SIGINT/EOF → 在飛 poll 回 `reason:"shutdown"` → `Session.Close()`
6. `internal/tools/status.go`：`discord_status`
7. 交給你跑：`go mod tidy && go build ./... && go run . --selftest`，輸出貼回 → 我修 → 記錄在 `_state/build-notes.md`
（M1 已交：`internal/files` + `discord/send.go` + `tools/send.go` + 單測；base64 入參不存在（已砍），入口 4 MiB 只留 `bufio.ErrTooLong` → exit 4 的防呆）
