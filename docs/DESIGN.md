# mcp-discord-roleplay — 設計 **v3（定稿候選）**

- 2026-11-16　v0→v1→v2→**v3**。v3：C1/C3/C4 拍定；**C2 我原先的說法是錯的，已改正**（4 MiB 是「入口」限制，出口 streaming 無上限）
- 狀態：**設計定案（v3.1）＋ M2c 已實作、輸出已極簡化，`go vet` / `go test` 全綠並已接入 live**。本檔是實作的依據。
- 實作進度：**M0 ✅**（config / router / bus / discord gateway client / `discord_status` / `--selftest`）、**M1 ✅**（`internal/files` 沙箱 + `discord_send` + 預算/間隔/斷路器 + 單測）、**M2a ✅**（per-agent Queue/Hub + `router.Audience` + `discord_poll`）、**M2b ✅**（media 三段式 inline/save/skip）、**M2c ✅**（target 語法收斂：`dm:` 單一字首、`dm:*`/`channel:*` wildcard、per-agent `read`/`write` 預設可收可放、閘門剩 `--allow-channels`/`--deny-channels`/`--unknown-channel`，`--allow-dm`/`--unknown-dm` 刪除）、**輸出極簡化 ✅**（2026-11-18：LLM 只拿必要欄位，其餘進 debug log；見 §3 的 wire 結構與判定三閘）、**`--tool-prefix` ✅**（2026-11-19：三個工具名由 prefix 組，預設裸名 `poll`/`send`/`status`；給模型看的 prose 也跟著走，由 `TestProseNamesFollowToolPrefix` 釘住）、**byte 旗標收斂 ✅**（2026-11-19：`--poll-max-bytes` `0` 報錯／`-1` 不設限，`--no-media-out` 刪除 ⇒ `--media-save-max-bytes=0` 是 save 的唯一開關）、**poll 預算改語意 ✅**（2026-11-20：`--poll-max-bytes` 只算 `text` 欄位的 byte（保險絲不是計費器）；附件與 embeds 永不因預算被丟（空殼 bug）；估價常數與 `attachments_total`/`attachments_omitted` 退場，`fitMessage` 不再重複 `json.Marshal`）、**`status` 改成狀態表 ✅**（2026-11-20：LLM 側只剩六個 key（`gateway`/`queue`/`dropped`/`last_error`/`targets`/`read_only`），2,658 B → ≈105 B（−96%）；`next_action`/`warnings`/靜態設定/host 路徑全數退場，完整面板改由 debug log 一行與 `--selftest` 承載；部署級事實搬進 tool description；見 §7.1）、**版本標籤收斂 ✅**（2026-11-20：`tools.BuildMilestone` 整個刪除；`Deps.Version` 與面板 `meta.version`/`meta.milestone` 一併退場（LLM 沒必要知道）；`main` 的 `version` 改 `var version = "0.1.0-dev"`，只出現在 `-version`、啟動 log（`logger.Info("starting", "version", …)`）與 MCP 的 **自定義 `serverInfo.version`**（`mcp.Server`）；MCP 的另一個 version——**protocol version**（`mcptransport/rpc.go`：`2026-07-28`／`2025-11-25`）是全然不同的東西、動不得；日後由 GitHub Actions 以 `-ldflags -X main.version=` 注入，因此它是 `var` 不是 `const`）。**`ok` 全面退場 ＋ send 變成心跳 ✅**（2026-11-20：`ok` 是 `isError` 的重複編碼、而且在成功路徑上是恆真常量（全 repo 沒人讀 `.OK`）⇒ poll／send／`errResult`／`fullPanel` 四處一併拿掉，失敗只剩 `{"error":"<code>",...}`，poll 空結果從 2 個 key 變 1 個；send 成功改帶 `queue`（與 status 共用 `queueRatio`）／`now`（與 poll 的 `ts` 共用 `tz.Format`）／`oldest_s`（新的唯讀 peek `bus.Queue.Oldest()`，空 queue 時不出現），順手成為 E-6 幽靈的觀測儀）。**自家 echo 與 lifecycle notice 退出佇列 ＋ poll 空手會自報原因 ✅**（2026-11-21：admission 就丟掉 `Author.Self`（`Hub.NoteSelfEcho()` 獨立計數，不與 `skipped_no_audience` 混用）⇒ `queue` 那個數字從此只計「別人對你說的話」，`include_self` 與 `msgWire.self` 一併退場，E-6 那顆幽靈從源頭消失；notice 改走 `Hub.WakeAll()`／`Queue.Wake()` 純喚醒（`PublishAll` 刪），不再佔 slot、不再擠掉未讀、不再計進 `dropped`；空手回執新增 `waiting`（依數量排序的拒收類別 `bot`／`mentions`／`target`／`channel`／`from`）＋ `oldest_s` ⇒「沒人說話」與「有話但不是給你的」一次分清，不用換 filter 再呼一遍才知道）、**貼圖與自訂 emoji 進內容 ✅**（2026-11-19：`normalize` 讀 `StickerItems`、以及把文字裡的自訂 emoji 碼補成附件，兩者在 `text` 留佔位（`[sticker:gaze]`／`<:gaze:img@N>`，而 **`N` 就是那一筆的 `content_index`**：尾號由 poll 組裝時定案，沒拿到 bytes 就退回原始碼 ⇒ 一份答案只有一個編號）、附件由 id 直接拼 CDN URL；emoji 一律 inline（`--media-emoji=inline|text`；每則上限 8 種、**不佔 `--inline-max-count`**），貼圖另有 `--media-sticker=inline|save|skip`（預設 inline，**蓋掉** `--media-image`）；新增 `internal/media/cache.go` 的 in-memory LRU（id-named 的 URL 不變 ⇒ 同顆 emoji 全 process 只下載一次）；見 §5.1。⚠ 本則日期取自實際時鐘，上面 11-20／11-21 兩則是先前憑感覺寫的，M4 統一校正）live smoke 已跑兩輪（紅項只有入站圖片的 ctx timeout，已修並 live 複驗），M3 = history/channels，M4 = README。
- 2026-11-19　**F-1b：DM 的座標就是 peer ✅**（`normalize()` 把 DM 的 `Event.Target` 寫成 `dm:<peer>`（guild 仍 `channel:<id>`、`ChannelID` 保留給審計）；peer 優先 state cache、miss 時退回 `Author.ID`，`"?"` 不造假 ⇒ **內部表示＝routing 表示＝LLM 表示**，poll 印出的地址永遠可直接喂回 `send`，未來開 `dm:*` 不致三種寫法混用；`eventTargets` 順手去重；見 §2）
- 2026-11-19　**F-e：APNG 貼圖的 URL 不該是 `.apng` ✅**（`addStickers` 原本拿同一個副檔名同時做**檔名**與 **CDN URL**，APNG 就拼出 `stickers/<id>.apng`，而 sticker CDN **沒有這個端點** ⇒ poll 只回 `media:"url" reason:"error"`，agent 看到 `[sticker:名]` 卻沒圖。實測同一 id：`.apng` **404**／`.gif` **415**（代表 CDN 是照副檔名清單認、不是即時轉檔）／`.png` **200**，而那 451,995 bytes 就是那張 APNG 的本體。`stickerFormat` 因此改為回三個值：檔名 `.apng.png`（對 LLM 誠實：bytes 是動畫 PNG、save 下來不會被當成靜圖）、URL `.png`（對 CDN 誠實）、mime `image/png`；見 §5.1）
- 2026-11-19　**F-f：save 的檔名分兩家 ✅**（**上傳檔**留 `<8 hex>-<safe name>`：上傳是「說給某個 agent 聽的」，別的 agent 拿不到 URL，只剩猜檔名一條路，所以那個 per-process secret 前綴是真的隔閘；**emoji／貼圖**改成 `<id><ext>`、不加 token：id 本身就是公開的 CDN 位址，換到的是**跨重啟、跨 agent 都沿用同一個檔**（同張貼圖不再每次重啟重抓、也不再長孤兒）。副檔名沿用 ingress 給的那串（APNG 貼圖 ⇒ `861289848563957801.apng.png`）；同顯示名不同 id 的貼圖靠 id 分隔，不再靠 hash 幫忙。`wire` 上的 `name` 欄位不變，只有 `local_path` 的檔名換制；見 §5）
- 2026-11-19　**F-4：滯留提示不再只發給空手 ✅**（`waiting`／`oldest_s` 原本**只在「一則都沒拿到」時才算**（`emptyHanded()` 是整個 repo 唯一的計算點），所以拿到 1 則、還有 2 則被 `targets` 擋住的 poll 只回 `remaining:2`：模型**看不出那 2 則是被誰擋的**，要嘛白呼、要嘛換 filter 試。閘門從「手上有沒有東西」改成「佇列裡有沒有東西」（`remaining > 0` 就同行），非空手那條路因此**開始算**這兩個數字（抽了顆 nil-safe 的 `oldestWaited(q)` 兩條路共用）。兩條語意釘進註解與工具描述：**`waiting` 只統計這一輪掃過且被擋下的** ⇒ 可小於 `remaining`（是 scope 不是遺失）；**`oldest_s` 是最老一筆的等待秒數、不區分被誰擋** ⇒ 與 `waiting` 不必對齊。`st.stopped` 那條「請立刻再 poll 一次」的 note 保留：它講的是**被額度切斷**，與被過濾是兩件事（`TestPollLimitCutLeavesWaitingOut` 釘住這個區別）。空佇列的成本不變（仍然只有 `reason` 一個 key）；見 §3）
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
    Kind string; Ts time.Time; Time string  // "message"（| "system"：已不進 ring，見下方 notice）；Time = Discord 原樣 ISO8601（帶時區偏移）
    Target string        // 內部座標：guild 頻道 "channel:<id>"、DM "dm:<peer>"（見下方 F-1 條）；poll 時再換成 agent 認的 alias
    GuildID, ChannelID, ChannelName string; DM bool
    MessageID string; Author Author{ID,Name,Tag string; Bot,Self bool}   // Self 在 admission 就用掉了，不上線
    Text string; Truncated bool; MentionsSelf bool
    Attachments []Attachment{ID,Filename,ContentType,Kind string; Size,Width,Height int; DurationSecs float64; URL,ProxyURL string}
    Embeds []Embed{Type,Title,Description,URL string}; Extra map[string]string   // "peer"=DM 對方 ID 等雜項
}
```
- **沒有 Seq、沒有游標**：取走就是 ack（LLM 端零狀態）。重啟丟 queue 是**設計上就決定這樣的**，補舊訊是 M3 `discord_history` 的工作
- 每個在 routing.json 聲明的 agent 一條 `Queue`（`--bus-capacity`，預設 2000/**agent**，環形覆蓋最舊並累計 `dropped`）；`Hub.Fanout(ev, agents)` 把**同一個指標**塞進多條 queue
- **自家 echo 在 admission 就被丟掉**（`Author.Self` ⇒ `Hub.NoteSelfEcho()` ＋ debug log，不 Fanout）：那句話是 `discord_send` 已經交到模型手上的東西，進 queue 只是把同一個事實再讀一次。它過去佔一個 slot、計進模型看到的 `queue` 數字、ring 滿時還會擠掉別人尚未讀的訊息，而預設 filter（`ignore_bots`）根本不會把它交出去 ⇒ 從入庫端解決，而不是叫模型多一個參數去要。『我剛才說了什麼』是 M3 `history` 的工作
- **lifecycle notice 只喚醒、不排隊**（`Hub.WakeAll()` ⇒ 每條 queue 的 `Wake()`：關掉再換掉 notify channel，不 Push、不動 `total`/`dropped`）：ready/resumed/disconnected 是「有東西發生了，起來看一眼」，不是「有人說了什麼」。不損失交付：斷網期間真正推進來的訊息本來就會自己喚醒 poll
- **貼圖與自訂 emoji 在正規化時補成 attachment**（`internal/discord/emoji.go`）：Discord 把這兩種訊息的內容放在 `content` 之外——貼圖在 `sticker_items`、自訂 emoji 只在文字裡留一個 `<:name:id>` 而**不附任何附件**（穿插在句子中間也一樣）——不補的話模型看到的是「一則沒有文字也沒有附件的訊息」。補完照樣走 §5 的政策；`bus.Attachment.Origin` 記住它是 `emoji` 還是 `sticker`（空＝Discord 傳來的上傳檔）。⚠ 這些是 `normalize` 造出來的，不是 Discord 給的，所以 `Size`/`Width`/`Height` 是 0
- **DM 的座標就是 peer**（F-1，2026-11-19，user 裁「用 b 一次到位、統一正規化」）：`normalize()` 對 DM 一律把 `Target` 寫成 `dm:<peer-user-id>`（guild 頻道仍是 `channel:<id>`；`ChannelID` 永遠留 DM 頻道 id 給審計）。理由：routing 對 DM 的寫法本來就是 `dm:<對方 id>`（§8：`dm:` 後面的 snowflake 是**使用者**不是頻道），而頻道 id 對模型是不可猜的噪音——舊做法下 poll 交出 `target:"channel:<dm id>"`，要 `discord_send` 接得回去才算能圓，而 `--unknown-channel`／per-agent ACL／未來 `dm:*` 這種擴充一旦碰到三種寫法混用就容易搞混。現在**內部表示＝routing 表示＝LLM 表示**，poll 印出來的地址永遠可直接餵回 send（round-trip）。**peer 有備援**：優先 state cache 的 `dmPeer()`；miss（陌生人第一則、或我們不在的共同 guild）時退回 `Author.ID`——1:1 DM 的作者就是 peer，不是我們臆造的事實；`"?"`（無 author 也無 webhook）不造假 peer、`Target` 就留 `channel:<id>`。副作用：`eventTargets()` 會把同一個 `dm:<peer>` 收到兩次（`Target` 一條、DM 分支一條）⇒ 已去重。單測：`TestNormalizeDMTargetIsThePeer`、`TestNormalizeDMPeerFromMissingOrWebhookAuthor`、`TestEventTargetsDedupesCanonicalDMLabel`
- ⚠️ **共用指標紅線**：`Push` 之後 Event 唯讀。任何 per-queue 的處理結果（M2b 的 `Media/LocalPath/ContentIndex`）只能放在 `tools` 的輸出結構（`pollAttachment`）；放進 `bus.Attachment` 會讓第二條 queue 讀到別的 agent 的值（例如 local_path 指向別人的檔）。已照此拆開，用型別讓編譯器擋死
- 醒來機制 `close(notify); notify = make(chan struct{})`（`sync.Cond` 無法配合 ctx/timer select）；`Push` 非阻塞
- gateway handler 內只做正規化＋**非阻塞** append，絕不在 handler 呼叫 REST
- `Drain(want)` 在鎖內跑 filter（`want` 必須是純計算，不得 I/O）；被 filter 掉／超出 limit／超出 byte 預算的，用 `RequeueFront` 原序放回，不丟
- Intents（MVP）：`IntentsGuilds|IntentsGuildMessages|IntentsDirectMessages|IntentsMessageContent`（**Message Content 可用 `--no-message-content` 關掉**，見 §8）；Handlers：`MessageCreate/Ready/Disconnected/Resumed`

---

## 3. `discord_poll`（M2a 定稿）
**語義**：從**呼叫者自己的 queue** 一次取走最多 `limit` 則、text 總量不超過 `--poll-max-bytes`（只算 `text` 欄位的 byte，見下方 byte 預算）的訊息，**取走就從 queue 消失**（drain 語意 = ack，不需要游標）；queue 空就掛住，**任何一則符合條件的新訊息抵達的瞬間立刻回傳**（不累積、不等安靜期），最長等 `max_wait(+jitter)` 秒。
（同一瞬間連發的多則，因為是「一次讀光 queue 頭」，自然會包在同一包裡回。）

```json
{"type":"object","properties":{
 "max_wait":{"type":"number","description":"等待秒數，合法範圍 5..600，預設 45。期間一有新訊息就立刻回傳；超過時間則回傳空結果(reason=timeout)。實際值會加 +0..15% 隨機抖動（只往後，不會縮到 client timeout 以下）"},
 "targets":{"type":"array","items":{"type":"string"},"description":"只收這些路由 alias 或 channel:<ID> / dm:<ID>；省略=本 agent 全部可讀 target。命中 filter 但沒送出去的訊息會留在 queue 裡下次再取"},
 "channels":{"type":"array","items":{"type":"string"},"description":"進階：直接指定頻道/DM/使用者 ID（仍需過 ACL）"},
 "limit":{"type":"integer","description":"單次回傳事件上限，預設 20，hard cap 100（其餘留 queue 下次讀）"},
 "mentions_only":{"type":"boolean","description":"true=只回 @我 或直接 DM"},
 "from":{"type":"array","items":{"type":"string"},"description":"作者過濾：user ID 或名稱"},
 "ignore_bots":{"type":"boolean","description":"預設 true（其他 bot 與 webhook；自家的訊息在 admission 就被丟掉了，永不進 queue）"}
}}
```
（`mcptransport.Schema` 沒有 min/max/default 欄位 ⇒ 範圍與預設只能寫進 description，並由 handler 驗證。）

`max_wait` 校驗：`<5` 或 `>600` → `isError`：`{"error":"bad_max_wait","min":5,"max":600,"default":45,"hint":"..."}`（**不提供 `max_wait=0`**；補舊訊走 M3 的 `discord_history` — 見 C1）

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
{"reason":"messages",
 "messages":[{"target":"boss","author":"阿明","author_id":"222333444555666777","ts":"2026-11-16T19:20:31+08:00","age_s":12,
   "text":"晚安","mentions_self":true,
   "attachments":[{"name":"night.png","kind":"image","url":"https://cdn.discordapp.com/...",
      "media":"base64","content_index":1}],
   "embeds":[{"title":"..."}]}],
 "remaining":7,"dropped":1,"notes":["..."]}
```
- **有滯留就要講，key 的數目就是區別**：真正沒人說話 ⇒ 恰好 1 個 key `{"reason":"timeout"}`（quiet server 是常態，而每一個 byte 都會進模型的 context；成敗是 tool result 自己的 `isError`，不再有個欄位複述它）；有東西留著 ⇒ 只要 `remaining` 出現，`waiting`／`oldest_s` 就**同行**，**不管這一次有沒有也拿到訊息**（F-4；空手是 `{"reason":"timeout","remaining":3,"waiting":"3 channel, 2 bot","oldest_s":184.6}`，非空手是 `{"reason":"messages","messages":[…],"remaining":2,"waiting":"2 target","oldest_s":4}`）。`waiting` 是**依數量排序的拒收類別**（`bot`／`mentions`／`target`／`channel`／`from`，同數則比名字），講的是「這一問的 filter 不收這些」，而且**只統計這一輪真的掃過的那些** ⇒ 可以小於 `remaining`（差額是掃完才到的，或是乾脆被額度切斷的——後者 `waiting` 整個不出現，改由 `notes` 講）。`oldest_s` 是最老那則等了幾秒（與 send 的 `oldest_s` 同一個算法），**不區分是被誰擋的** ⇒ 與 `waiting` 的計數不必對齊。差別在於模型要不要換個 filter 再呼，而不是白重呼一次
- **預算只算 `text` 欄位的 byte 數**（`messageCost(m) = len(m.Text)`）：它是**保險絲不是計費器**，目的只有一個：防止一次 tool result 把沒有裁切機制的 harness 的 context 直接灌爆、整個 session 掛掉。JSON 骨架（key、author、url、notes）刻意不計：實測一則訊息骨架 144 B、一個附件 entry 159 B，把它們折進預算就必須每裁一步重 `json.Marshal` 一次（舊版 `fitMessage` 主迴圈每圈 2 次、外加 `messageCost` 各一次），白瞎了 `encoding/json` v2 的底層設計。inline 的 base64 在 content block 裡、本來就不在這段 JSON 裡；模型是按 token 收錢（一張圖 ~1000 token），不是按 JSON byte ⇒ 也不佔預算（舊版那個 `result_bytes` 固定點迴圈就是為了湊一個實際上沒人在用的數字，已整段拿掉）
- `reason` ∈ `messages|timeout|disconnected|cancelled|shutdown`；**timeout 不是錯誤**（tool result 的 `isError` 為偽，舊版的 `ok:true` 講的就是這件事，現在不再多一個欄位複述）。沒有訊息且 gateway 不在聽時，`timeout` 會被改寫成 `disconnected`（對一條死線路說「沒人說話」是誤導）；`cancelled`/`shutdown` 保留自己的字，它們講的是「這一問其實沒送到你手上」
- **超額的處理**：裝不下的整則留 queue（下次 poll 照原序先拿到，`notes` 講一聲）。單則 text 就超過整個預算 → 按 **rune 邊界**把 `text` 裁到整整預算（不預留骨架、不預留 note 空間），標 `text_truncated:true`，`notes` 寫「這一則 text 太長（原本 N bytes），裁到 M bytes 只留開頭」，該則**照樣送出**（反死鎖：queue 頭那一則若不裁就永遠卡住）。裁斷的那一則不會下一輪再出現（已被 Take 走），這是預期行為。附件與 embeds **永不因預算被丟** ⇒ 不會再出現只有 id 沒有內文的空殼訊息
- `remaining`（**只在 >0 時出現**）＝ 佇列裡還留著幾則（**包含被這次 filter 拒掉的那些**，它就是 `Drain` 傳回的尾數） ⇒ 只有被截斷（`notes` 會講一聲）或被 filter 拒掉（`waiting` 講是哪幾類，**拿到訊息時也照給**）才值得立刻再呼一次。（舊版那個 `partial:true` 布林已退場，只進 log）
- `dropped` 只在 >0 時出現：那是「沒人 poll 把 queue 擠爆、被覆蓋掉的舊訊息」，要直接告訴模型
- `target` 標的是**這個 agent 認得的 alias**（`router.AliasForTarget` 在組裝時現算）；沒有 rule 收養它就留座標——DM 標 `dm:<peer>`（能直接回話的形式），其餘 `channel:<id>`。wildcard rule 的 alias（`dm:*` 的 `dms`）**不是地址**，所以也不拿去當前綴。同一個 Event 在兩條 queue 裡可以標不同名字，所以這個欄位只能長在 `pollMessage` 上
- 作者壓成一條字串 `author:"阿明"`，名字取的是 **Discord 自己會顯示的那個**：`Message.Member.Nick`（群內暱稱）優先，沒有才退回 `User.DisplayName()`（`global_name` → `username`）——只讀 `DisplayName()` 的話，操作者改了群內暱稱而 agent 永遠叫錯人。`bus.Author` 的 `tag`（舊式 discriminator）對回話沒有增量 ⇒ 進 log；空名字才退回 `tag`，不給匿名台詞。唯一的 ID 例外是 `author_id`（見下方判定三閘）：它過第②閘，因為 `discord_send` 的 `to` 吃 `dm:<ID>` ⇒ 沒有它，agent 無法回 DM 給「剛講話那個人」，只能用 routing 檔裡的 alias；`buildEvent` 的佔位 `"?` 不上線（回 `to:"dm:?"` 是個陷阱）。
- 舊版會「先裁 text、再從尾丟附件、最後丟 embeds」，並用 `attachments_total`／`attachments_omitted` 兩個欄位交代；現在只有 text 是變數，那兩個欄位與三條估價常數（圖 1000／其他 300／embed 200）一起退場。別跟 `dropped` 混：那是**整則訊息**被 queue 擠爆覆蓋掉
- **附件在輸出裡只有三種形態**（`policy` 不上線，只告訴模型你**拿到**了什麼）：`media:"base64"` + `content_index`（≥1：`content[0]` 是這段 JSON，圖 block 接在後面）／`media:"file"` + `local_path`（可直接餵 `discord_send` 的 `files[].path`）／`media:"url"` + `reason`（見 §5 詞表）。附件在線上只有 `name`（原 `filename`）、`kind`、`url`、`media`，加條件欄位 `duration_secs`（僅音訊／視訊，那是唯一的內容描述）／`reason`／`local_path`／`content_index`；`id`、`mime`、`size`、像素邊長、實際交付位元組數全進 log
- **文字裡的佔位就是 content block 的索引**（詳 §5.1）：貼圖在 `text` 尾加 `[sticker:gaze]`；自訂 emoji 碼原地改寫成 `<:gaze:img@1>`（每則最多 8 種、依 id 去重；`a:` 前綴照 Discord 保留），**`img@N` 的 `N` 就是那一筆附件的 `content_index`**（一份答案只有一個編號）。block 地址要等 poll 組裝時才算得出來 ⇒ ingress 寫的只是暫存號，`relabelPlaceholders` 在 `applyMedia` 之後改定。**沒拿到 bytes 的 emoji 不佔號**，退回原始碼；佔位號也因此跟批次走。同一顆 emoji 出現兩次只下載一次、兩個佔位指同一個 N（重複本身也是資訊）。佔位在 `--text-max-runes` 截斷**之後**才追加，不會被切一半
- **上線欄位的判定三閘**（往後新增欄位一律先過這三閘）：①要用它回話 ②要據它決定下一步 ③是敘事的必要輸入。ID、時間戳、位元組數、像素尺寸、alias 背後的真實頻道名都是「誰在何時算了什麼」⇒ 那是 log 與 `discord_status` 的活（完整面板約 90 個 key，現在只進 debug log 與 `--selftest`；`discord_status` 給模型的是 §7.1 那六個 key，所以便宜到每輪都能呼）。目前唯一的 ID 例外就是 `author_id`
- 被搬走的欄位沒有蒸發：`-log-level=debug` 時 poll 打一條 `poll`（`agent`、`waited_ms`、`max_wait`、`connected`、`self`、`count`、`remaining`、`partial`、`bytes`、`dropped`、`notes`、`result=`整個 wire），send 打一條 `send`（`agent`、`target`、`kind`、`channel_id`、`parts`、`message_ids`、`delivered`、`created_at`）。預設 log level 下兩者都被 `Enabled(Debug)` 守門 ⇒ 零成本
- **payload 的 JSON 不 escape HTML**（`jsonResult` 改用 `Encoder` ＋ `SetEscapeHTML(false)`）：Go 預設把 `<`／`>`／`&` 寫成六位元的 `\u003c`，而這份 payload 全是它們（佔位、`<@id>` 提及、CDN url 的 query）；payload 本身又是 envelope 裡的一個字串 ⇒ 那層 escape **多活一層**，只看一層的人讀到 `\u003c:gaze:img@1\u003e`，而且每個佔位在 `--poll-max-bytes` 那種緊預算下白燒十個 byte。transport（`mcptransport/server.go`、`stdio.go`）的 encoder 預設不動 ⇒ 影響面只有我們自己的 payload；由 `TestJSONResultKeepsAngleBracketsAndAmpersandsRaw` 釘住

---

## 4. `discord_send`（M1 已實作）
```json
{"type":"object","required":["to"],
 "properties":{
  "to":{"type":"string","description":"目標：alias（路由表）/ channel:<ID> / dm:<ID> / #名稱 / @名稱 / me(傳給自己的 DM) / last(本 agent 最近說話的 target)"},
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
{"queue":"0/2000","now":"2026-11-19T01:52:03+08:00"}
```

加條件欄位：`parts_sent`（**只在 >1**）、`attachments:[{"name":"pose.png","size":12345}]`（只在有掛檔）、`url_count`（只在 >0）、`dry_run`（只在 dry-run）、`warnings`（只在有話要講）、`oldest_s`（**只在 queue 非空**：佇列最前面那則等了幾秒，小數一位）⇒ 純文字一則的 send 就是 `queue` ＋ `now` 兩個 key，而 dry-run 是四個（`dry_run,now,queue,warnings`：那條 warning 正是「別以為真的貼出去了」的依據，所以它上線）。

**固定欄位是「一句話的脈絡」**：send 是模型說完話的瞬間，此時它真正要問的不是恆為 true 的 `ok`，而是「我說的這段期間有沒有人說話」（`queue`）、「最老的等了多久」（`oldest_s`）、「現在幾點」（`now`，要 wait 到某個時段時用得上）。`queue` 與 status 共用同一個產生器（`queueRatio`）⇒ 兩處永不漂移；`now` 用與 poll 的 `ts` 同一個 formatter（`tz.Format`）⇒ 兩串時間可直接比對；`oldest_s` 由新的唯讀 peek `bus.Queue.Oldest()` 現算（`time.Since(ev.Ts)`，不是建檔時的快照）且不消耗任何東西。**副作用（好）**：`queue` 不歸零而 `oldest_s` 一直長大 ⇒ 就是 E-6 那顆卡住的幽靈。
失敗（`isError:true`）：`{"error":"<code>","hint":"<下一步>","retry_after_ms":...,"http_status":...,"discord_code":"...","breaker_open":true,"target":"boss","kind":"channel","channel_id":"...","parts_sent":1,"message_ids":[...],"part_failed":2,"delivered_before_failure":1}`
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
| `save` | 下載寫入 `--media-out` 目錄（全程 `os.Root`；**檔名分兩家**：**上傳檔** `<8 hex>-<safe name>`，token 由「每 process 亂數 secret ＋ attachment id ＋檔名」哈希而來 ⇒ 外部猜不到（上傳是「說給某個 agent 聽的」，別的 agent 拿不到 URL，只剩猜檔名一條路），同 process 可重現；**emoji／貼圖** `<id><ext>`、**不加 token**（id 就是公開的 CDN 位址、CDN 又不問密碼，藏它沒意義；換到的是**跨重啟、跨 agent 都能沿用同一個檔**，副檔名沿用 ingress 給的那串 ⇒ APNG 貼圖就是 `861289848563957801.apng.png`）；**同名檔已存在就直接沿用，不再下載**） | `media:"file"` + `local_path`（=`--local-path` 前綴＋檔名，可直接回喂 `discord_send`）+ `url` |
| `skip` | **不下載**（零 I/O） | `media:"url"` + `reason` ＋ `url`（要圖再自己抓；`proxy_url`/size/mime 都只進 log） |

**預設（定案）**：`image=inline`、`audio=skip`、`video=skip`、`other=skip`。（音訊依 C3 決定 skip，需要 STT 時你自己改 mode 即可）
**超過上限怎麼辦（M2a 定案：不降級）**：`inline` 碰到任一上限 → 直接 `skip`，由 `reason` 講清楚是哪一條。不自動退 `save`：降級鏈會讓同一張圖在不同輪的形態飄移，模型無從判斷，而 save 也是要 I/O 的。
**上限共通規則**：`--inline-max-*` 與 `--media-save-max-bytes` 一律 **`-1`＝不設限、`0`＝該模式停用**，其他值＝上限。`--poll-max-bytes` 只有 `-1`＝不設限這半邊，**`0` 直接報錯**（reply 預算沒有「停用」這狀態；舊版讓 0 也等於不設限，是同義字造成兩個 sentinel，已收斂）。
**能力上限（不是降級）**：MCP 的 content block 只有 `text`/`image`/`audio`/`resource_link`（見 `mcptransport/content.go` 的 encoder），**沒有 video block** ⇒ `--media-video=inline` 得到 `media:"url"`＋`reason:"unsupported"`。
**`reason` 詞表**（只在 `media:"url"` 出現）：`policy`（設定就是 skip）、`unsupported`（該 kind 沒有 content block）、`too_big`（超過單檔 bytes／save 上限）、`side`（邊長超過 `--inline-max-side`，我們不縮放）、`no_budget`（超過每包 `--inline-max-count`／`--inline-max-total-bytes`）、`save_off`（設定要 save 但 save 已被 `--media-save-max-bytes=0` 停用，或目錄開不起來）、`error`（抓檔／寫盤失敗；細節在 log，不上線）。
`save` 的清理：開機跑一次 `media.Cleanup`（先 `--media-keep` 天數、再 `--media-max-mb` LRU by mtime），只碰 regular file，子目錄一律跳過。
**timeout 的擁有權在讀端**（live smoke 抓出來的坑）：每檔 15s 由 `Session.fetchCtx()` 建立，`get`/`open` 只收 ctx、**自己不 `defer cancel()`**；cancel 跟著 `inline()`/`save()` 直到 body 讀完。原因是 net/http 把 response body 綁在發 request 那個 ctx 上，headers 一到就 cancel 會讓之後的 `Read()` 直接拿到 `context canceled`——本機 httptest 的 8 byte payload 連 header 同段進 buffer，所以單測摸不到，真 CDN 82 KB + TLS 第一個 chunk 就紅。

### 5.1 貼圖與自訂 emoji（內容不在文字裡的兩種訊息）

兩者在正規化時補成附件（`internal/discord/emoji.go`），URL 由 id 直接拼、**不需要 REST**（CDN 是公開靜態檔案）：

| | 佔位（進 `text`） | 附件 | 政策 |
|---|---|---|---|
| 貼圖（每則最多 3 張） | `[sticker:gaze]`（`content` 有字就接在後面） | `name=<貼圖名>.<ext>`（APNG 是 `名.apng.png`）、`url=stickers/<id>.<png|json|gif>`、`origin=sticker` | `--media-sticker=inline|save|skip`（預設 `inline`；**蓋掉** `--media-image`，因為那個是給照片設的）|
| 自訂 emoji（**穿插在句子裡或獨占整則都算**） | 每個碼原地改寫成 `<:gaze:img@1>`（依 id 去重、每則上限 **8 種**，多出的留原始碼） | `name=<id>.png`（animated `<id>.gif`）、`origin=emoji` | **一律 inline**，開關是 `--media-emoji=inline|text`；不受 `--media-image=save` 影響 |

- **為什麼 emoji 不跟 `--media-image`**：自訂 emoji 的名字常常不是人取的（`emoji_23`，或乾脆就是它被抓下來時那個 `<id>.png`），所以「給你一個 URL／一個檔名」等於什麼都沒說——**圖本身就是訊息的內容**。`text` 那半邊是「我要省流量，碼留著就好」：正規化根本不造附件，一個 request 都不花。
- **每則 8 種、而且是獨立的預算**：一句話裡穿插多個自訂 emoji 是常態（不是例外），所以不做「整則只有 emoji 才處理」這種窄閘。上限是**去重後的種類數**、寫成常數而不是旗標（你說的「後面若不夠再來加設定去調整」）。它**不佔 `--inline-max-count`**（`checkCaps` 對 `origin=emoji` 免計數、`s.inlined` 也不加），否則 4 張照片會把訊息的圖擠光、反之 8 顆 emoji 會把照片擠光。位元組那兩條（`--inline-max-bytes-image`／`--inline-max-total-bytes`）照樣管——那是最後的保險絲，觸發時該筆變 `media:"url"`＋`no_budget`，而它的佔位退回原始碼（見下條）。
- **副檔名是兩件事，不是一個**（F-e，live 打出來的）：`format_type` 1→名 `.png`／URL `.png`、**2→名 `.apng.png`／URL `.png`**、3→`.json`（Lottie）、4→`.gif`。教訓是 **sticker CDN 沒有 `.apng` 端點**：同一個 id 實測 `.apng` → **404**、`.gif` → **415 Unsupported Media Type**（代表 CDN 是照副檔名清單認、不是即時轉檔）、`.png` → **200**，而那 451,995 bytes 就是那張 APNG 的本體。⇒ 檔名對 LLM 說實話（bytes 是動畫 PNG，save 下來不會被當成靜圖），URL 對 CDN 說它聽得見的語言；兩者允許不同，`stickerFormat` 因此回三個值（`fileExt`／`urlExt`／`mime`）。**APNG 我們宣告 `image/png`**（APNG 就是多了幾個 chunk 的 PNG，模型 API 吃得下；代價是動畫變成第一格）；Lottie 是一個 JSON、不是圖 ⇒ `kind=other`，對它 `inline` 只能回 `media:"url"`＋`unsupported`（沒圖可給）。
- **id-named ⇒ 可以記在記憶體**：Discord 不換一個 id 後面的 bytes（改名＝id 不變、重傳＝新 id），所以同一個 URL 的答案永遠一樣；上傳檔沒有這個性質（那是 CDN 上一條可以被換掉的路徑）而且體積大 ⇒ 不入 cache。`internal/media/cache.go`：LRU **256 條／8 MiB／單檔 512 KiB**、鍵＝URL、mutex 保護（`Fetcher` 全 process 共用，多個 agent 可能同時摸同一顆 emoji）。**只有 inline 路徑會填**：save 已有自己的重用機制（**上傳檔**＝同 process 內檔名可重現；**emoji／貼圖**＝檔名就是 id，所以跨重啟也成立 ⇒ 檔案在就是答案），而且是 stream、不經記憶體。
- **`img@N` 就是 `content_index`：一份答案只有一個編號**（F-c，user 裁示「直接唯一化固定」）：舊做法是佔位號在 **ingress** 就烙死（＝該則 `attachments` 的位置），而 `content_index` 是 **poll 組裝時**算的 block 地址，於是多則訊息一起 poll 時 `img@1` 會在同一份答案裡指到兩個不同的圖（live §J 現場發生）、模型會讀錯。現在 `applyMedia` 之後跑 `relabelPlaceholders`：拿到 bytes 的 ⇒ 佔位尾號改成它的 `content_index`；**沒拿到的 ⇒ 佔位退回原始碼**（`<:cheat:1209375944377237586>`，自帶 id、天然對得上那一列）。三件事只能取兩件（①只剩一個數字 ②那個數字就是 block 位置 ③沒拿到 bytes 也有編號），我們犧牲 ③。所需事實由 `bus.Attachment` 兩個**純內部**欄位 carry（`Placeholder`＝塞進 text 的那整串、`Raw`＝Discord 原始碼；`json:"-"`，不上線、`attWire` 零變更）。**代價**：佔位號跟批次走（同一則訊息單獨 poll 是 `img@1`、排在兩則之後變 `img@4`）——認可過的取捨：一個 agent 一則訊息只會讀到一次。**實作細節**：改寫用一次 `strings.NewReplacer`、不是逐附件 `ReplaceAll`——兩個 guild 可以有同名 emoji（`<:gaze:111>`／`<:gaze:222>`），前一次的改寫結果可能正好是後一次要搜的 literal，就會被改兩次、指錯檔（`TestPollSameNameHandlesDoNotCollideWhenRelabeled`）。貼圖不參與（`[sticker:名]` 本來就不編號）。改寫只動 `pollMessage.Text`（per-call 副本），**從不動 `bus.Event.Text`**（那是 fan-out 共用指標）
- 待辦：`--emoji-cache`（把這批 bytes 存盤、啟動時自動載入）在 backlog；本輪先 in-memory。

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
- MVP：`discord_status`（見 §7.1：**六個 key 的狀態表**）、`discord_channels`（可見 guild/頻道/DM + alias⇄ID 對照；模型靠它拿到能貼的 ID）、`discord_history`（REST ≤100 則、before/after，輸出格式同 poll 的 message；也是「補舊訊」的唯一路徑）
- P2：`discord_typing`（**M2a 決定 poll 不做 typing**：掛住語意與它衝突）、`react`、`edit`、`delete`、threads、progress 心跳（多 agent 遠端時才有意義）
- 註：bot 無「標已讀」API（只有 thread read-state）→ 不做

### 7.1 `discord_status`：一張狀態表，不是報告（2026-11-20）

**為什麼改**：舊面板實測 **2,658 bytes / 117 條 key path**（≈750 token），其中靜態 88 個、活的 29 個，而那 29 個裡 15 個是 `0`/`null`/空字串；同一個 key 講兩次有 5 處（`file_roots`、`max_files_per_msg`、`max_upload_bytes`、`unknown_channel`、`agent`）；host 路徑洩漏 3 處。一個「偶爾才呼」的診斷面板被當成每輪都要讀的狀態來源，尺寸就長錯了。

**LLM 側（`statusWire`，六個 key）**：

```go
type statusWire struct {
	Gateway   string   `json:"gateway"`              // ready | starting | down | stopping
	Queue     string   `json:"queue"`                // "unread/capacity"，恆出現
	Dropped   uint64   `json:"dropped,omitempty"`    // ring 覆蓋累計，只有這個 agent
	LastError string   `json:"last_error,omitempty"` // 斷線原因，有才出現
	Targets   []string `json:"targets,omitempty"`    // send 收的 alias
	ReadOnly  []string `json:"read_only,omitempty"`  // 收得到、發不出去
}
```

```
{"gateway":"ready","queue":"0/2000","targets":["boss","ami"],"read_only":["log"]}      ≈ 105 B
{"gateway":"ready","queue":"1500/2000","dropped":12,"targets":[…]}                    ≈ 130 B
{"gateway":"down","queue":"3/2000","last_error":"websocket: close 1006","targets":[…]} ≈ 165 B
```

**沒有提示欄位**：`next_action`、`warnings[]`、`hints[]` 全部退場。理由同 §3 的判定三閘——一個會讀比值的人不需要句子告訴它「佇列快滿了」：`"queue":"1500/2000"` 就是那句話；`"gateway":"down"` 就是「別發，先做別的」。判斷留給呼叫者，server 只陳述事實。`ok` 也不給（tool result 本來就有 `isError`）。2026-11-20 把這條從 status 升為**三個 tool 共通規則**：`ok` 全面退場（poll 與 send 的成功回執不再帶它、`errResult` 也不再寫 `ok:false`），因為它是 `isError` 的重複編碼，而且在成功路徑上是個恆真常量。

**`gateway` 一個欄位吃掉四個布林**（`started`/`connected`/`ready`/`stopping`），優先序由外而內：`stopping` → `!Started || !Connected` ⇒ `down` → `Connected && !Ready` ⇒ `starting` → 否則 `ready`。從未啟動與 socket 死掉對模型是同一件事（發都會失敗），差別留在面板。順帶修掉舊 `nextAction` 的真 bug：它用 `queueSizeTotal(qs)`（**所有** agent 的總和）判斷「有貨」，多 agent 時會叫一個佇列空的人去 poll；新的 `queue` 一律是呼叫者自己的。

**完整面板去哪裡**（`fullPanel`，就是舊的那包）：
- **debug log**：`logStatus` 一條 `status`，`lg.Enabled(ctx, slog.LevelDebug)` 守門 ⇒ 預設零成本，開了才 marshal。
- **`--selftest`**：tools 清單後印 `--- <prefix>status ---`，內容是 `StatusPretty(d)`＝`wire:` 一行 ＋ indent 的完整面板。selftest 從不啟動 client ⇒ 那裡會顯示 `down`，順帶當狀態機的煙霧測試。
- **host 路徑**（`file_roots`、`media.save_dir`）只活在面板；`send` 的 `not_found` 回執也拿掉了重複的 `file_roots` 欄位（訊息句子裡已經有）。

**warnings 那 15 條的歸屬**：三族變成欄位（queue／dropped／gateway）；其餘分流——

| 性質 | 去處 |
|---|---|
| 部署級、啟動就該看到（intent off、dry-run、file root 是 `.`/`/`、附件全 skip、完全沒有 queue） | `--selftest` 散文＋`Audit()` |
| session 級、模型只需知道一次（intent off ⇒ `poll`、dry-run ⇒ `send`） | **tool description**（`deploymentNote`，由 `RegisterAll` 依 config 追加；description 每 session 傳一次，payload 每呼一次） |
| 純操作者視角（`skipped_no_audience`、`self_echoes`、跨 agent dropped、routing.json 未列名、`self_id` 未知、斷路器細節） | debug log |

**不在這裡報的**：send 額度／限速／斷路器。真撞上時由 tool result 自己講原因（`rate_limited`／`budget_exhausted`／`circuit_open` ＋ `retry_after_ms`）——那是失敗路徑的責任，不是狀態面板的。

**`targets` 是 routing 檔的 alias 名單**，不含 `me`／`last` 兩個魔術字（它們不是地址，是指令）；也不含 wildcard 的 alias（`dm:*` 的「dms」發過去會被 `wildcard_target` 拒）。

---

## 8. 設定
優先級 flag > env > `--config routing.json` > 預設
| 鍵 | 預設 |
|---|---|
| `DISCORD_TOKEN`（`--token-env` 指定變數名）| 必填；**只讀 env/file**，log redact，自動加 `"Bot "` |
| `--agent` | `default` |
| `--tool-prefix` | `""`（空＝工具就叫 `poll` / `send` / `status`）。註冊時才貼上去（`mcptransport.NewRegistry(prefix)`），工具自身宣告的是裸名；client 自己會加 namespace 時保持空即可。舉例給模型看的**文字**（description／error hint／`instructions()`）一律從同一個值組出來（`tools.ToolName` ＋ `Deps.toolStatus/toolPoll/toolSend`），所以換 prefix 不會留下叫錯名的幽靈工具；改動由 `TestProseNamesFollowToolPrefix` 釘住 |
| `--config` | routing.json（見下） |
| `--transport` | `stdio`（`--http-addr` 127.0.0.1:8787、`--origin`、未來 `--auth` per-agent token） |
| `--poll-min-wait / --poll-max-wait / --poll-default-wait / --poll-jitter-pct` | 5 / 600 / 45 / 15 |
| `--bus-capacity` | 2000（**每個 agent 各一條**）|
| `--poll-max-bytes` | `16384`（16 KiB；**只算各則 `text` 欄位的 byte 總和，不是 JSON 大小**，見 §3；`-1`＝不設上限，**`0` 報錯**；數字不做範圍管制，太小是自己找的）|
| `--media-image / --media-audio / --media-video / --media-other` | `inline / skip / skip / skip` |
| `--media-emoji` | `inline`（另一值 `text`）。文字裡的自訂 emoji 要不要圖；它不是 MediaMode、也沒有 `save` 這選項（見 §5.1）|
| `--media-sticker` | `inline`。貼圖的政策，**蓋掉** `--media-image`（那個是給照片設的）；`save`／`skip` 照 MediaMode 語意（見 §5.1）|
| `--media-out` | `mcpdr-media`（**cwd 底下**）；留空只允許搭 `--media-save-max-bytes=0`，否則啟動報錯（忘記給路徑）|
| `--local-path` | `""`＝由 `--media-out` 推。給 LLM 的 `local_path` 前綴：宣告「`--media-out` 這個目錄在 `--file-root` 眼裡叫什麼」，bind mount 也對得上 |
| `--media-save-max-bytes` | `4 MiB`（單檔落地上限；`-1` 不設限、**`0` 是關閉 save 的唯一開關**（`--no-media-out` 已刪）。Discord 允許到 ~200 MB，超限不降級，直接 `media:"url"`＋`reason:"too_big"`）|
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
貼圖：`Message.StickerItems []*StickerItem`，而 `StickerItem{ID,Name,FormatType}` 是「最小可渲染物件」——**沒有尺寸也沒有 description**（那些在完整的 `Sticker` 上，要 REST 才拿得到）；`StickerFormat` 1=PNG／2=APNG／3=Lottie／4=GIF。尺寸為 0 不是 bug：`checkCaps` 的 side 閘只拒「能證明太大」的，所以這些補出來的附件不會被邊長誤殺。
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
bus（ring 覆蓋 dropped/Notify 不漏/Wake 不進 ring/Drain-RequeueFront 原序/nil-Hub 防呆/唯讀契約）、audience（規則優先序、一 event 多 agent、stray、deny、unknown-channel）、router（規則優先序、ACL 拒發、unknown-channel、wildcard 退回、per-agent grant 覆寫）、target（`dm:`/`channel:`/alias/`#名`/`@名`/`me`/`last`；模糊名回候選不瞎猜）、files（路徑政策：先 `path.Clean` 再把開頭 `/` 當根（`./a/../b` == `b`）、`..` 倖存者/`~`/volume name 拒；symlink 逃出 root 被 `os.Root` 擋、根內可用；多 root 優先序；目錄/超大/數量上限；mime 四層優先序與 sniff 後 offset 歸零；`ValidateURL`）、send（seam 注入假 poster + 假時鐘：AllowedMentions.Parse 為空 slice、gap/預算/斷路器狀態機、小 `retry_after` 本地重試且**重試前有 Rewind**、 breaker open 時完全不碰 REST、half-open 只放 1、DryRun 不碰 REST 不佔額度、classify 全碼）、tools（`-dry-run` 端到端 envelope、各 `to` 拒因與 `targets` 候選、切段邊界 1999/2000/2001/CJK/附件僅末段、`text_too_long`、`last` per-agent 隔離、部分送達）、tools（`jsonResult` 不 escape `<>&`、不帶尾換行、key 仍排序）、poll（假事件序列 → envelope 斷言、text 預算裁斷（**附件永不因預算被丟**＝空殼回歸）、裁切不切壞 UTF-8、queue 隔離、cancel 後訊息仍在、空答兩種（真閒置恰好 1 個 key；有滯留則帶 remaining＋waiting＋oldest_s；**拿到訊息時也同行（F-4）**）、**wire key 集合契約**（envelope/message/attachment/embed 四層各自釘死，新增欄位要先過 §3 三閘））、gateway admission（自家 echo 不進任何 queue、`self_echoes` 與 `skipped` 分開計）、**DM 的 `Target` 正規化為 `dm:<peer>`**（`TestNormalizeDMTargetIsThePeer`／`TestNormalizeDMPeerFromMissingOrWebhookAuthor`：cache miss 退作者 id、`"?"` 不造假、webhook 用 webhook id；`TestEventTargetsDedupesCanonicalDMLabel`）、discord 正規化（貼圖→附件＋`[sticker:..]` 佔位、自訂 emoji（穿插或獨占）→`img@N` 原地改寫／去重／每則上限 8 種／`--media-emoji=text` 零附件、碼被截斷一半就不配對、**正規化 gate 的正則不得比 `parseEmojiToken` 嚴**（兩者不一致時 gate 會靜默否決解析器 ⇒ 整則不配圖）、animated→`.gif`、APNG/Lottie 的分類、**APNG 的 URL 是 `.png` 而檔名是 `.apng.png`**（`TestNormalizeAPNGStickerFetchesThePngRendition`：sticker CDN 沒有 `.apng` 端點，live 實測 404）、**佔位尾號由 poll 改成 `content_index`**（`TestPollRelabelsEmojiHandlesToBlockIndex`／`TestPollRevertsHandleWhenBytesDidNotArrive`／`TestPollSameNameHandlesDoNotCollideWhenRelabeled`））、media（三段政策：inline 真的進 content block、save 一檔不重複下載、skip 零 request、超限不降級、**headers/body 分段交付也要讀得完**（E-8 回歸）、cleanup 年齡＋LRU、**CDN cache：同一個 id 兩輪只下載一次、上傳檔不入 cache、emoji 無視 `--media-image=save` 仍 inline、**貼圖照 `--media-sticker` 三值（Lottie 的 inline＝unsupported）**、**save 的檔名分兩家**（上傳帶 token、emoji／貼圖用 id ⇒ 跨重啟沿用同一個檔：`TestSaveNameOfCDNObjectsIsTheId`／`TestSavedStickerIsReusedAcrossProcesses`）、**emoji 不佔 `--inline-max-count`（雙向）**、過大的單檔不收**）、status（**wire key 集合契約**（六個釘死＋退場欄位斷言不在：`ok`/`next_action`/`warnings`/`hints`/`connected`/`ready`/`dry_run`/`breaker_open`/`send_budget_left`/`uptime_s`/`file_roots`）、健康時 ≤ 200 B 防迴歸、`gateway` 狀態機全組合（含 stopping 優先）、`queue` 是未讀/容量比值且分母不為 0、`dropped` 只在非零、`targets`/`read_only` 按呼叫者 ACL、**wildcard alias 不當地址**、`last_error` 只在有、完整面板進 debug log、部署級事實進 description、`StatusPretty` 兩視）、`--selftest`（真連煙霧測試）。
**測試資料紀律**（2026-11-19，user 裁）：真實的 **user／channel id 與帳號名不進 code**（一律用合成 snowflake，例如 `700000000000000001`），live 的真 id 只進 `_state/`；emoji／sticker 這類「內容 id」可留真值（反查不出身分，只暴露公開的 CDN 圖與 snowflake 的建立時間）。

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
