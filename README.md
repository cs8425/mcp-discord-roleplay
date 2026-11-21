# mcp-discord-roleplay

把 Discord 變成 LLM agent 的聊天室：一支 **MCP server**（stdio），用 Discord gateway ＋ REST 收發訊息，讓模型以「一個會看、會講、會翻舊訊的角色」接進語音以外的文字頻道。設計與取捨的理由全在 [`docs/DESIGN.md`](docs/DESIGN.md)；這份只講**怎麼跑起來、跑起來會看到什麼**。

一個 process 服務**一個 agent**（`--agent`）：自己的佇列、自己的 ACL。要多 agent 就開多份 process（共用 token 可以，共用 queue 不行——queue 在記憶體裡）。

## 六個工具

名字會被 `--tool-prefix` 在前面加字串（例如 `-tool-prefix dc_` ⇒ `dc_poll`）。給模型看的說明文字也跟著改，不會出現「工具叫 `dc_poll`、內文卻教它呼叫 `poll`」。

| 工具 | 做什麼 | 消耗佇列？ |
| --- | --- | --- |
| `poll` | 等並取走這個 agent 佇列裡的新訊息（**一次只答一個頻道**）；空手會自報原因 | ✅ 取走就沒了（被 filter 擋下的、以及別的頻道的留著） |
| `send` | 發一則（可帶附件）；超 2000 字自動分段 | — |
| `status` | 六個 key 的狀態表（gateway / queue / dropped / last_error / targets / read_only） | — |
| `history` | 讀一個頻道的舊訊息（REST）：上線前的那段、以及被我們 ACL／filter 擋掉的 | ❌ 完全不消耗 |
| `get` | 讀單則（REST）；附件要抓下來就用它 | ❌ |
| `channels` | 目錄：Discord 看得到的 guild／頻道 ＋ 我們自己的位址表（哪些能寫、哪些只能收） | ❌ |

`history`／`get`／`channels` 走 **REST 不走 gateway** ⇒ 可在任何時刻插入、不影響 `poll` 的游標，也看得到那些「我們不讓它進 queue」的東西。

## 需求

- **Go 1.27**（`go.mod` 指定；CI 與 release 都從 `go.mod` 抓版本）。
- 一個 **Discord application ＋ bot token**。
- 兩個 Go module：根 module 與 `./mcptransport`（`replace` 指向本地）⇒ `go test ./...` 之外，改動到 transport 時也要 `(cd mcptransport && go test ./...)`。

## 快速開始

```bash
cp .env.example .env                 # DISCORD_TOKEN=...（見下）
cp routing.example.json routing.json # 把你的頻道／DM 填進去
go build -o mcp-discord-roleplay .

# 先不要接 client，自己檢查一遍（不發任何 request，也不起 gateway）
./mcp-discord-roleplay -selftest

# 什麼都不真發，只看決策（路由、沙箱、分段、envelope）
./mcp-discord-roleplay -dry-run -log-level=debug
```

接進 MCP client（以 `command` ＋ `args` 的形式；token 一律走 env，不寫進 argv）：

```json
{
  "mcpServers": {
    "discord": {
      "command": "/absolute/path/to/mcp-discord-roleplay",
      "args": ["-tool-prefix", "dc_", "-config", "routing.json", "-history-max-bytes", "4000"],
      "env": { "DISCORD_TOKEN": "..." }
    }
  }
}
```

**stdout 只能是 MCP 訊息**：log 預設走 stderr（`-log-file` 可改成檔）；`-log-file=stdout` 只會在你確定沒有 client 在讀那條 pipe 時才有意義。

## Discord 那邊要開什麼

我們只訂閱四個 intent（`internal/discord/client.go` 的 `Intents`）：

- `Guilds` — 頻目錄，讓 alias⇄ID 查得到。
- `GuildMessages` — 群裡的訊息。
- `DirectMessages` — DM。
- `MessageContent` — **唯一的 privileged intent**，在 Developer Portal 的 Bot 頁開啟。

⚠ **Portal 那顆開關和我們的旗標管的不是同一件事**（這條是 live 踩出來的，代價很高才看懂）：

- **訊息的 `text` 有沒有，是 Portal 那邊的 Message Content 設定決定的**，不是我們的旗標決定的。同一支 binary、同一組旗標，Portal 開著 ⇒ 不 @ 任何人的 guild 訊息照樣有全文；Portal 關掉 ⇒ 非 @ 的 guild 訊息在 `poll`／`history`／`get` **三條路徑一起**只剩 id／author／ts（`text` 與 `attachments` 一起消失）。
- `--no-message-content` **只影響我們 IDENTIFY 時宣告不宣告那個 bit**。不宣告的好處是「Portal 那邊的狀態怎麼變我都連得上」，代價依文件是拿不到 content——但如上所述，實測 Portal 開著時不宣告也有 text，所以**這個旗標不是 content 的開關**，別拿它當除錯的第一把手。
- 三類例外（Portal 關掉時**照樣有** `text`）：**被 @ 的訊息**、**DM**、**自己發的**。前兩條 live 驗過。
- `history` 整頁 `text`／`embeds`／`attachments` 都空時會加一條註記講這件事，但它**只在掛了 `--no-message-content` 時才會出現**（`read.go:222` 那道閂，故意留著）：掛著旗標 ⇒ 「空是因為我們沒要」剛好對；沒掛旗標而 content 又空（＝ Portal 關了，gateway 也被 disallowed-intents 擋在門外）⇒ `history` 仍會答，只是**全空而無註記**，那時候看 row 只剩 id／author／ts 這個形狀就知道了。
- ⚠ 「Portal 沒開卻硬要宣告 bit 會被踢」這件事**我們至今沒實測過**，連 close code 都還有兩個候選（官方表 4013／舊紀錄 4014）。真要驗，會看到的是**斷線重連迴圈**——因為目前程式對 WS close code 沒有特判，不會有一句「Portal 沒開 MESSAGE_CONTENT」。
- 門檻（兩邊文件講的不一樣）：docs 說 verified app 要 100+ guilds 核准，**Portal 寫的是 10,000+ 使用者需審核** ⇒ _bot_ 長大的過程中有一天會被要求審核，不是加入第 101 個群就斷。

## routing.json

`--config` 指定的位址表（缺檔不報錯，只用 `--allow-guilds`／`--allow-channels` 那套預設閘門）。範例見 [`routing.example.json`](routing.example.json)：

```json
{
  "agents":  [ { "id": "default", "targets": ["boss", "ami", "log", "dms"] } ],
  "targets": [
    { "alias": "boss", "id": "channel:111222333444555666", "read": true, "write": true },
    { "alias": "log",  "id": "channel:333444555666777888", "read": true, "write": false },
    { "alias": "dms",  "id": "dm:*",                        "read": true, "write": false }
  ],
  "unknown_channel": "ignore"
}
```

- **`alias` 是模型要貼的東西**，`id` 是真身（`channel:<snowflake>` 或 `dm:<使用者 id>`）。`dm:*`／`channel:*` 是 wildcard，指的是一個**類**不是地址。
- **`read` 決定收不收得到、`write` 決定能不能 `send`**，兩邊預設都偏保守（`log` 那種「看得到但沒人回話」的頻道就 `read:true, write:false`）。per-agent 可覆寫全域預設。
- `unknown_channel`：沒命中任何規則的頻道要 `ignore`（不進 queue）還是 `ring`（照進）。
- **ACL 是在 admission 做的**：被 ACL 擋掉的訊息根本不會進 queue，所以 `history` 看得到它們、`poll` 看不到。
- 真 id 只進 `routing.json` 與 `_state/`，**不寫死在 code 裡**（測試一律合成 snowflake）。

## 常用旗標（全表與理由在 `docs/DESIGN.md` §8）

| 旗標 | 預設 | 註 |
| --- | --- | --- |
| `--token-env` | `DISCORD_TOKEN` | 只吃環境變數名，不吃字面值 |
| `--agent` | `default` | queue ＋ ACL 的名稱空間 |
| `--tool-prefix` | 空 | 接進自己會命名空間的 client 時常要 `dc_` |
| `--config` | `routing.json` | 缺檔靜默跳过 |
| `--env-file` | `.env` | 缺檔靜默跳过；`LoadDotEnv` 走 `os.Setenv` 且**不覆蓋**既有環境 |
| `--transport` | `stdio` | `http` 還沒做（`--http-addr` 先佔位） |
| `--no-message-content` | false | 見上一節：只管 IDENTIFY，不管 content 有無 |
| `--nick-lookup` | `cache` | `cache`＝讀取側補作者暱稱（先查 state，miss 才補打一次並寫回）；`off`＝退回全域名、零 request |
| `--poll-min-wait`／`--poll-max-wait`／`--poll-default-wait` | 5／600／45 | `poll` 的 `max_wait` 合法區間與預設；只加**往後**的抖動（`--poll-jitter-pct=15`），不會縮到 client timeout 以下 |
| `--poll-max-bytes` | `16384` | 一次 poll 的預算，**只算 `text` 與引用的 byte**（保險絲不是計費器）；`-1` 不設限 |
| `--reply-max-bytes` | `80` | 一則回覆裡「對方講的那句」開頭幾個 byte 上線；`0`＝只給作者與 id，`-1`＝不裁（見 DESIGN §4.6）|
| `--bus-capacity` | `2000` | per-agent 佇列；滿了丢最舊的，`status` 的 `dropped` 會漲 |
| `--history-max-bytes`／`--channels-max-bytes` | `4000`／`4000` | 整個 response 的額度；超過從**最舊**那端丢＋註記。LLM 不能自己調 |
| `--read-timeout` | `10s` | 讀取側單次 REST 截止；讀取路徑**關掉** discordgo 的 429 自動重睡 |
| `--media-image`／`--media-video`／`--media-other`／`--media-audio` | `inline`／`skip`／`skip`／`skip` | 進件附件三段政策 `inline\|save\|skip`；`--media-emoji`／`--media-sticker` 可各自覆寫 |
| `--media-out`／`--local-path`／`--media-keep`／`--media-max-mb` | `mcpdr-media`／derive／`168h`／`512` | `save` 的落點、告訴 agent 那個盤從哪裡看、開機清多舊、LRU 上限 |
| `--inline-max-count`／`--inline-max-bytes-image`／`--inline-max-side`／`--inline-max-total-bytes` | `4`／`4MiB`／`1280`／`12MiB` | inlined 圖片的四道閘（我們**不縮圖**，太大就跳過）；emoji 不佔 `--inline-max-count` |
| `--file-root`（可重複）／`--no-file-root` | cwd／off | `send` 能讀的本地目錄；路徑是**相對這個 root** 的名字，經 `os.Root` 開 ⇒ `..` 想逃出 root 會擋（root 內可用），`~` 不展開 |
| `--max-upload-bytes`／`--max-files-per-msg` | `8MiB`／`10` | 送出側 |
| `--max-sends-per-min`／`--send-gap-ms`／`--breaker-errors`／`--send-retry-wait-ms` | `20`／`250`／`3`／`1500` | 預算、同頻道最小間距、連續 rate-limit 錯誤數→60s 斷路、願意自己等的最長 `retry_after` |
| `--allow-guilds`／`--allow-channels`／`--deny-channels` | 空 | `routing.json` 缺席時的閘門；`deny` 贏過 `allow`，且**連讀都一起擋** |
| `--tz` | `Asia/Taipei` | 所有吐出去的時間戳帶時區 |
| `--log-level`／`--log-file` | `info`／`stderr` | `debug` 才有每支 tool 的 `api_ms`、暱稱補打的成敗等 |
| `--dry-run`／`--selftest` | false | 前者只決策不發；後者印報告就退出 |

## 跑起來會遇到的事（都是實測過的）

- **「連回來」不等於「那段沒漏」**。gateway 斷掉後能不能 resume 有一個窗口：實測 **32.6s 斷線可 resume（不漏）**、**128.6s 已經回不去（那段真的漏）**。漏掉的內容要用 `history` 補，`status` 的 `gateway` 欄位只告訴你現在連不連得上。
- **bot 的線上狀態不判健康**。Discord 顯示「線上」不等於我們的 gateway 活著（反之亦然）；要健康就看 `status`，或看 log。
- **上傳檔的 URL 會過期**（帶 `?ex=&is=&hm=` 簽名，約 24 小時），而 sticker／emoji 的 CDN URL 是永久的。⇒ `history` 只列 url；**圖要留，就趕快 `get`**（或把 `--media-image` 設 `save`）。
- **作者的顯示名字**：群裡用暱稱（`tapris`）、DM 用全域名（Discord 本來就這麼給），兩側都帶 `author_id`——那才是唯一可靠的同一人身分。讀取側的暱稱是快取＋按需補打（`--nick-lookup`），所以大 guild 的第一頁可能慢一點（`api_ms` 會把這段算進去），而且**改過暱稱的人在下一次發訊息之前我們不會知道**（我們不訂閱 `GUILD_MEMBER_UPDATE`）。
- **`poll` 空手回來有兩種**：真的閒置 ⇒ 只有 `reason`；有東西被擋 ⇒ 帶 `remaining` ＋ `waiting`（按閘門計數：`bot`／`mentions`／`target`／`channel`／`from`／`other_channel`）＋ `oldest_s`。`waiting` 只計這一次掃過的，所以可能小於 `remaining`——那是範圍，不是遺失。
- **`poll` 一次只答一個頻道**（2026-11-22）：從佇列最老那則開始，把**同一頻道**的全收齊（照舊受 `limit`／`--poll-max-bytes` 節制），別的頻道的留給之後幾輪。答案頂層的 `target` 講的是這批訊息出自哪個對話（row 不再逐則帶 `target`／`dm`），而且可以直接餵回 `send`；沒有訊息的答案不帶 `target`。被留下的那些計進 `waiting` 的 `other_channel`，不丢不亂序。想自己選頻道就用現成的 `targets`，所以**沒有新旗標**。
- **`channels` 過濾器吃 alias**（`poll` 的 `channels` 參數）：ID、Discord 實名、我們的 alias 都認（大小寫不拘，`#` 與 `channel:` 前綴兩邊都剝）。這條是 2026-11-19 的 live 教訓——頻道的 Discord 實名跟我們的 alias 不同名時，舊實作會**無聲擋掉全部**，看起來像沒人說話。
- **自訂 emoji 的佔位碼**：`<:name:img@N>`，`N` 就是這個 response 裡附件的 `content_index` ⇒ 句子裡的碼和圖不會變成兩個對不上的地址。佔位是**附加**不是覆蓋，所以有 content 時它與真文字共存。
- **自家的訊息永不進 queue**（`send` 已經把答案交給呼叫者了），所以 `poll` 不會自我回音；要看自己講過的要 `history` ＋ `include_bots:true`。
- **讀取失敗不會借用送出側的詞**：`no_access`／`unknown_channel`／`cancelled` 這些在讀取路徑上說的是讀，不會對你說「我們試著發」。

## 開發

```bash
gofmt -l .                                   # 空＝乾淨
go build ./... && go vet ./... && go test ./...
(cd mcptransport && go build ./... && go vet ./... && go test ./...)
go test -race ./...                          # CI 的 ubuntu job 跑這個
```

- 不需要 token 的東西全部是單測：REST 用 `discord.ReadHooks` 腳本化、gateway 用注入的假 poster ＋ 假時鐘。
- `--selftest` **也不碰網路**：它只驗證設定（含 routing、file roots、旗標互斥），印出工具清單與狀態面板就退出；gateway 在 selftest 裡**從來不啟動**，所以輸出永遠是「no gateway connection was attempted」，`gateway` 那欄讀作 `down` 是預期的。
- 輸出欄位是**契約**：`poll`／`status`／read row 各有 key 集合測試釘住，要新增欄位先過 DESIGN §3 那三道閘。
- **回給模型的字串一律英文**（工具描述、envelope 值、`notes`）；中文留在程式註解與 docs。這條由 `internal/tools/language_test.go` 釘住：掃本套件所有非測試檔的字串字面量，冒出 Han 字元就 fail。

## 還沒做／已知限制

- **`http` transport 尚未實作**（`--transport` 只收 `stdio`；`--http-addr`、`--origin` 先佔位）。
- `channels` **不列 threads**（它是頻道的狀態，不是目錄），也**不列 DM**：v0.29.0 沒有 `/users/@me/channels` 的包裝 ⇒ DM 那半邊由 `routing{}` 講。
- **大 guild（≥250 人）的第一頁**：`GUILD_CREATE` 不帶 `members[]`、我們又沒訂 `GUILD_MEMBERS`（privileged），所以那些作者的名字要靠按需補打；補打失敗時退回全域名，**不改答案**。
- **`--media-*-save` 的檔名分兩家**：CDN 物件（emoji／sticker）用 id ⇒ 跨重啟沿用同一個檔；上傳檔帶 token ⇒ 每次上傳是新檔。
- **WS close code 沒有特判**（見上文 intents 節）。
- `.github/workflows/` 的 `ci.yml` 與 `release.yml` 原本是從隔壁專案（chrome-mcp）複製來的，**已改成本專案**：build 根目錄的 `.`（我們的 `main` 不在 `./cmd/`）、版本變數 `-X main.version=`、artifact 名 `mcp-discord-roleplay-*`。CI 覆蓋 ubuntu＋windows 雙平台（`-race` 只在 ubuntu，Windows runner 缺 C toolchain）、gofmt／vet／build／test 四步，外加一支 `go mod tidy` 必須無 diff 的衛生檢查；✅ **兩支都已在 GitHub Actions 真跑過**（2026-11-20）：首跑 ubuntu 綠、windows 兩紅，都是**測試自己的 OS 假設**（`filepath.Base("//x")` 在 Windows 讀成 UNC server；`Fetcher.Dir()` 是 `filepath.ToSlash` 形態），改斷言後 → **雙平台全綠**（`-race` 照樣只在 ubuntu）。`release.yml` 只在 push tag 時跑 ⇒ 仍待第一次發版才算驗畢。
- 多 agent 的端到端預演、拔網線 30s 重連的例行驗證，仍在 M4 範圍內（見 DESIGN §11）。

## 授權

Apache License 2.0（見 `LICENSE`，`Copyright 2026 cs8425`）。授權條文主體是官方原文、一字未改，只有附錄（Appendix: How to apply this License）那段 boilerplate 的佔位字換成了年份與著作人。

本 repo **沒有**在每個 `.go` 檔頂部放授權頭（Apache 官方建議、Go 社群多數只做 `LICENSE` ＋ README），要補的話是巨量機械式修改，列在 `_state/backlog.md`。
