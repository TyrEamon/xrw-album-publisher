# XRW VPS Publisher

VPS 常驻发布端。它从 Veil 的图集列表发现任务，按单张图片保存 SQLite 断点，将图片转存到 Telegram Channel 图床，最后生成完整图集发布包并可选提交到 xrw-album 的私有管理接口。

## 为什么用 Go

- 可交叉编译成一个 Linux 二进制文件，VPS 不需要 Node/Python 运行环境。
- 常驻内存占用低，适合长期限速下载、重试和代理轮换。
- SQLite 使用纯 Go 驱动，不依赖 VPS 上的 CGO 或系统 SQLite 开发包。

## 数据边界

Telegram Channel 保存图片本体。VPS直接调用 Bot API 的 `sendMediaGroup` / `sendDocument`，随后把公开键、`file_id`、`file_unique_id`、`message_id` 和图集快照一起提交给管理 Worker。Worker 将映射写入 D1 的 `tg_files` 表；`album_details` 保存公开图片 URL、宽高和图集信息。Bot Token 不会进入网页 URL。

VPS 的本地 SQLite 会记录：

- 图集发现状态、源站更新时间、预期数量和失败原因。
- 每张源图片的顺序、宽高、TG公开URL和图床返回的TG标识。
- 图集详情返回的标签；发布时同步到 D1 的标签关系表，当前展示端不读取。
- `pending / incomplete / ready / ok / blocked` 等断点状态。
- 普通错误连续失败5次后进入 `failed` 隔离队列；运行 `retry-failed` 可在后续批量补录。

## TG 频道图床

创建 Bot、把它设为所有目标频道的管理员，然后配置 `TG_BOT_TOKEN` 与逗号分隔的 `TG_CHAT_IDS`（单频道仍兼容 `TG_CHAT_ID`）。新图集按源图集 ID 固定分配到一个频道，并把频道 ID 存入本地 SQLite；已有上传断点的历史图集会保留在列表第一个频道，不会在续传时跨频道。发布器把原文件按每10个一组发送；末尾2–9个仍是一组，只剩1个时使用单个 `sendDocument`。文件不会按照片模式压缩，也不经过 `telegra.ph/upload`。

每个图集第一组的第一个文件带频道说明，格式为 `#标签` 加 HTML 引用样式的图集名称；没有标签时只写图集名。后续文件组不重复说明。标签中的空格和标点会转换成下划线，重复标签会去掉。

每张图片使用稳定公开键 `veil-<source_image_id>`，最终展示地址形如：

```text
https://album.example.com/file/veil-1191867
```

管理接口的发布包同时包含 `photos` 与 `tg_files`。后者供 Worker 写入 D1 文件映射，示例：

```json
{
  "public_key": "veil-1191867",
  "url": "https://album.example.com/file/veil-1191867",
  "file_id": "telegram-file-id",
  "file_unique_id": "telegram-unique-id",
  "message_id": 123,
  "channel_id": "-1001234567890",
  "content_type": "image/jpeg"
}
```

展示 Worker 后续使用私密 Bot Token 调用 `getFile`，代理返回图片并做 Cloudflare Cache。不能把 Telegram 的临时下载地址直接写入 D1，因为地址含 Bot Token且不是永久链接。

D1 的 `tg_files` 只长期保存 `public_key / file_id / file_unique_id / message_id / content_type`。完整公开URL和相同的频道ID不会按图片重复落库；`album_details` 也使用紧凑格式保存图片路径、宽高和源图ID，由 Worker 对外返回时还原成原有JSON对象，以降低百万级数据量的占用。

## 配置

参考 [.env.example](./.env.example)。程序读取环境变量，不会主动读取 `.env` 文件；systemd 使用 `EnvironmentFile` 注入配置。

`VEIL_PROXIES` 填 SOCKS5 出口列表。VLESS、VMess、Trojan 客户端只需在 VPS 本地暴露 SOCKS5 端口，发布器不直接实现这些协议。每个代理拥有独立的限速时钟，默认按 `80 次 / 300 秒 / IP` 运行；403/429 后该出口冷却35分钟。

`TG_UPLOAD_INTERVAL=3500ms` 是每个频道各自的媒体组间隔；`TG_GLOBAL_INTERVAL=500ms` 控制同一个 Bot 的全局请求起始间隔，`TG_MAX_CONCURRENT=3` 限制同时上传数。每次请求最多包含10个文件。

包含 `.webp` 的批次会退化成逐张上传（每张一条消息，只有第一张带 caption）。原因是 Telegram Bot API 只看分片文件名和声明的 MIME 就给上传分类，`*.webp` 与 `image/webp` 会被归成贴纸，而媒体组的 `attach://document0` 只能引用 document，于是整组直接 400 `Wrong file identifier/HTTP URL specified`。改成逐张 `sendDocument` 并带上 `disable_content_type_detection=true` 可以让原始 WebP 字节、文件名和 `image/webp` MIME 全部保留；代价只是相册分组布局丢失，展示 Worker 只读取每个文件的 `document.mime_type`，不受影响。

标准 Telegram Bot API 的 `getFile` 下载上限是20MB，因此 `MAX_IMAGE_MB` 不能设得更大；否则频道虽然可能上传成功，展示 Worker 也无法把该文件取回。发布器会在上传前拦住超限图片，避免生成打不开的图集。

## 构建

在 `publisher` 目录执行：

```bash
go test ./...
go build -o bin/xrw-publisher ./cmd/xrw-publisher
```

Windows 上交叉编译 Linux amd64：

```powershell
$env:GOOS='linux'
$env:GOARCH='amd64'
$env:CGO_ENABLED='0'
go build -o bin/xrw-publisher-linux-amd64 ./cmd/xrw-publisher
```

## 首次运行

初始化数据库并查看状态：

```bash
./xrw-publisher status
```

扫描所有图集。接口每页100项，当前约需996次列表请求：

```bash
./xrw-publisher discover -pages 0
```

也可以从指定列表偏移量继续扫描，便于分段发现或恢复：

```bash
./xrw-publisher discover -pages 100 -offset 50000
```

列表中 `uploaded_images < image_count` 的图集会进入 `waiting`，不会反复请求尚未入库的图片。后续扫描发现源站已经上传完整时，它会自动转为 `pending`。

先试跑一个图集：

```bash
./xrw-publisher run -max 1
```

不配置 `XRW_ADMIN_URL` 时，完整图集进入：

```text
work/outbox/veil-<gallery_id>.json
work/outbox/veil-<gallery_id>.txt
```

状态为 `ready`，不会误报为已写入D1。配置管理接口后再次运行，会直接发布这些 `ready` 图集并改为 `ok`。

`status` 还会显示成功下载的源图片字节与成功上传的 Telegram 文件字节。VPS 账单口径应另外用 `vnstat` 或云厂商 NetworkIn/NetworkOut 指标确认：

```bash
vnstat
vnstat -d
vnstat -m
```

## 常驻运行

```bash
./xrw-publisher daemon -pages 5 -batch 0
```

每个周期扫描最新5页，然后把当前可处理队列持续跑空，再等待 `DISCOVERY_INTERVAL`。首次全量发现应单独执行一次 `discover -pages 0`。

部署模板见 [deploy/xrw-publisher.service](./deploy/xrw-publisher.service)。务必把 `/var/lib/xrw-publisher` 放在持久化磁盘上。

## GitHub Pages 增量快照

`snapshot` 命令只导出状态为 `ready / ok` 且自上次成功导出后发生变化的完整图集。配置 `GIMG_PUBLIC_BASE` 与 `GIMG_SIGNING_SECRET` 后，公开批次把 Telegram `file_id` 转成不可篡改的 `gimg` 签名地址；批次仍不包含原始 `file_id`。GitHub Pages 看图时由独立 `gimg` Worker 验签并取图，完全不查 D1：

```bash
./xrw-publisher snapshot -out /var/lib/xrw-publisher/github-snapshot/batches -max 1000
```

启用 `gimg` 后，旧快照需要一次性重导出，后续定时任务恢复增量模式：

```bash
./xrw-publisher snapshot -reset -out /var/lib/xrw-publisher/github-snapshot/batches -max 1000
```

若完整图集超过单批 `-max`，不要再次加 `-reset`，让后续定时运行继续导出剩余图集。仓库变量 `GIMG_PUBLIC_BASE` 只负责在Pages构建时把旧 Telegraph 地址改写为 `gimg` 反代；签名密钥只保存在VPS和Worker secret中，绝不放进GitHub仓库或快照。

配套的 `xrw-publisher-snapshot.timer` 每15分钟运行一次同步脚本；没有新增图集时不会产生提交。VPS只向 `snapshot` 分支追加脱敏批次，该分支推送不触发Pages。Pages工作流固定每3小时检出最新快照并部署，网站代码推送到 `main` 时仍会立即部署。

两套图片Worker相互独立：桌面的 `xrw-album-gimg-worker` 服务GitHub Pages，不绑定D1；仓库内 `cf-image-worker` 部署为 `xrw-album-cimg`，绑定现有D1并服务CF站。它们和根目录主站分别使用不同Worker名称，部署其中一个不会覆盖另外两个。

## Windows 本地写真上传器

本地已有一套写真时，不需要把它伪装成 Veil 爬虫任务。`xrw-local-uploader` 提供只监听 `127.0.0.1` 的本地网页：选择文件夹、填写标题和标签后，它会读取真实宽高，按自然文件名顺序把原图每10张一组以 Telegram 文件发送，并在整套成功后生成与 Pages 构建脚本兼容的脱敏快照。

上传器使用独立的 `local-uploader.db`，不读写 VPS 的 `publisher.db`，图集ID使用 `manual-*` 前缀。不同来源出现相同内容时允许重复，但不会互相覆盖。每组 Telegram 成功结果立即落库；程序或电脑中断后，重试只处理尚未成功的文件。

Windows 上在 `publisher` 目录运行：

```powershell
.\Start-Local-Uploader.ps1
```

也可直接双击 `Start-Local-Uploader.cmd`。上传器在可见终端内前台运行，关闭终端窗口或按 `Ctrl+C` 就停止，不再隐藏到后台；仅关闭浏览器不会停止服务。启动器会拒绝重复启动。已经保存的成功上传记录和快照会保留，上传过程中中断可能需要重试当前未完成批次。

首次运行会编译单文件 EXE，并打开 `bin/local-uploader.env`。需要填写：

- `TG_BOT_TOKEN` 与一个或多个 `TG_CHAT_IDS`。
- 独立图片 Worker 的 `GIMG_PUBLIC_BASE`。
- 与该 Worker 完全一致、至少32字符的 `GIMG_SIGNING_SECRET`。
- 可选：`XRW_WP_BASE` 指向要浏览的 WordPress 站，默认 `https://cosplaytele.com`；填 `off` 隐藏“站点图包”区域。要浏览多个站就用 `XRW_WP_SITES`，写成一行、用逗号分隔，每个站写 `url`、`id|url`、`id|name|url` 或 `id|name|url|取图方式`；两种都设置时以 `XRW_WP_SITES` 为准。取图方式可选 `media`（默认，走 `media?parent=`，适合 cosplaytele.com 这类把套图当附件上传的站）、`content`（从正文的 `<img>` 里取图，适合 misskon.com 这类 `media?parent=` 只返回一张封面、套图内嵌在正文里的站）或 `acgmhn`（该站没有 API，直接读分页 HTML，适合 acgmhn.com 这类自研 CMS）。

网页默认地址为 `http://127.0.0.1:8765/`。数据库和批次默认保存在 EXE 旁边的 `local-uploader-data`，因此把 `publisher/bin` 留在 D 盘就不会占用 C 盘项目空间。也可通过 `LOCAL_UPLOADER_DATA_DIR` 和 `LOCAL_SNAPSHOT_DIR` 指向其他绝对路径。

本机无法直连 Telegram 时，在私有 `bin/local-uploader.env` 中设置 `HTTPS_PROXY=http://127.0.0.1:10808`（端口改为代理软件的 HTTP/混合端口），然后重启上传器。仅开启 Windows 系统代理或浏览器代理不会自动配置 Go 程序；已有同名进程环境变量优先于配置文件。失败任务可点击“重试未完成部分”，无需重新创建。上传错误会隐藏 Bot Token，启动时也会脱敏旧任务报错，不更改上传进度；不要把私有配置提交到 Git。

生成文件位于 `local-uploader-data/batches/manual-snapshot-*.json`。上传器会通过当前电脑的 Git Credential Manager 自动把每个快照推送到主仓库的 `snapshot/batches`，推送成功后任务才显示“快照就绪”；Pages 在下一轮三小时定时构建时读取。网络或 GitHub 推送失败时，本地快照和 Telegram 上传断点都会保留，点击“重试未完成部分”只会重试发布阶段。公开快照只包含带签名的 `gimg` URL，不包含 Bot Token 或 Telegram `file_id`。上传页的“打开快照目录”可直接定位本地副本。

### 导入草稿

本地上传页的“导入草稿”区域是站点图包和 Telegram Web 两种来源共同的落地处。启动上传器后，先在“导入草稿”区域复制连接码，再点击“安装油猴脚本”。脚本只在 `https://web.telegram.org/` 运行，连接码保存在油猴脚本本地配置和上传器数据目录，不进入 Git 仓库。

在 Telegram Web 打开选择模式后，先点击需要导入的频道主帖。脚本会用主帖生成标题和标签；检测到 `Comments` / 评论入口时，再点脚本面板里的“读取该帖评论区图片”，脚本会打开对应评论区，并把其中已经加载的完整图片补进同一个草稿。页面当前已经加载的图片会被逐张写入本地草稿，视频预览会跳过；同一草稿中的完全相同文件会自动跳过。随后点击“本地编辑”，可以修改标题、分类和标签，点击图片排除不需要的内容，并拖动调整顺序，最后复用本地上传队列发布到频道和生成快照。

第一版不会自动翻页或读取尚未加载的评论。长图集需要先在 Telegram Web 滚动到相应消息，再逐条选择；这避免脚本依赖 Telegram Web 内部的私有接口。草稿阶段只保存到本机，可以从草稿卡片直接删除并同时清理暂存图片；建立正式上传任务后，为保证断点重试会暂时保留文件，任务成功生成快照时会自动删除对应草稿目录和本地图片，只保留任务记录。

### 站点图包

上传页的“站点图包”区域直接浏览旧站已发布的图包，勾选后点“导入所选”即可把原图抓成一份草稿，再走上面同一套编辑与发布流程。标题右侧的“收起”可以把整块列表折起来（只留状态行），选择记在浏览器里；配置了多个站时标题栏会出现站点下拉，切换站点会同时清空搜索、分类和勾选，因为分类编号和已勾选的图包只对原站点有意义。

索引只缓存元数据（标题、张数、日期、封面地址、分类），每个站点一份，存在 `local-uploader-data/site-albums/<站点 id>/index.json`。封面用 `<img>` 直接热链源站，页面本身不下载任何图片；只有勾选的图包会被抓取。上传器启动时若索引为空会走一次全量扫描，之后每 `XRW_WP_REFRESH`（默认6小时）做一次增量：只重新读取 `modified` 晚于上次构建时间的图包。按住 `Shift` 点“更新列表”可强制重建整份索引；每个站的刷新互不阻塞，某个站挂了不影响其他站。

大站（例如 4.9 万个图包的 misskon.com）全量扫描要十分钟以上，所以索引是边走边写的：第 1 页读完立刻落盘，之后每 25 页再写一次，状态行同时显示“正在读取第 N/M 页”和已读到的数量，列表在扫描过程中就能搜、能勾、能导入。重建时还没读到的图包先沿用旧索引里的那份，所以列表只会变多不会缩水。写了一半就被打断的索引会带上 `partial` 标记，下次刷新会忽略增量、直接重新全量扫描——否则那些没读到的图包会因为“修改时间不新”而永远补不回来。

图包标题由源站标题去掉“N photos”尾部得到。选 `media` 图片源时，附件列表按“文件名主干”分组，占多数的那个主干被当作这组图，其余混进来的别家附件会被丢掉；再用标题声明的张数校验一次，只有恰好等于“张数+1”时才把多出来的那张当作封面剔除。张数对不上时保留全部附件，宁可多一张也不漏图。选 `content` 图片源时不做这套分组：直接按正文里 `<img>` 的出现顺序取图（优先 `data-src`，其次 `src`），去重后每张一个条目，正文里放几张就导入几张。

`content` 图片源的源站标题经常夸大张数（misskon.com 的套图大多只在正文里放 12 或 24 张预览，标题却写着 111 张），光看标题分不清“完整”和“被截断”。所以列表把两种数都显示出来：读不到真实张数时只显示标题的数，量过之后对得上的显示真实数、对不上的显示“真实 / 标题”并在封面上挂一个“不全”角标。真实张数由 `POST /api/site-albums/verify` 提供，它把一页图包的 id 拼成一次 `include=` 请求（每批最多 100 个，接口每次最多收 120 个）读出正文里真实的图片数，结果写回索引的 `actual` 字段。已经量过的图包不会再请求；标题变了（张数或修改时间变了）就重新量。`media` 图片源的站点没有这个问题，接口直接返回空结果。

`acgmhn` 取图方式的站点（acgmhn.com 的写真栏）没有 REST API，索引和取图都读 HTML：列表是 `/cos/`、`/cos/index-N.html`，每页 36 条，条目的 `<span class="pagenum">` 就是真实张数（视频条的同一位置是 `00:07` 这种时长，会被过滤掉），所以这些站不会有“不全”的问题，也不提供 `verify`。取图走站点的轻量接口 `/ajax_cos/<id>[-N].html?ajax=1`，一次返回一张图的地址和翻页信息，站点因此把请求限到大约每秒一次：读取每张图之间有 1 秒间隔，429 和 5xx 会按 1/2/4/8 秒退避重试。这个站的封面和图片在 `m.acgnfl.com`，所以页面的图片策略会放宽到允许任意 `https:` 来源。全量扫描约 1,005 页、3.6 万个图包，按限速大约要跑几十分钟，但边走边写让它扫描期间就能用。

抓取按源站顺序并行下载到临时目录（最多4张同时），再按原顺序写进草稿，因此草稿里的编号和源站一致。某一包在源站缺图时整包回滚：草稿被删除，失败原因显示在该图包的卡片上，可以重试。同一图包不会重复导入。

封面热链要求浏览器能访问源站；本机需要通过代理访问时，同时给浏览器和上传器设置代理（上传器读 `HTTPS_PROXY`，浏览器读系统代理）。源站地址会按需加入页面的 `img-src`，不需要为它放宽其他 CSP 指令。

## 旧 Telegraph 图库迁移

`xrw-legacy` 使用单独的 `legacy.db`，不会读写 Veil 发布器的 `publisher.db`。首次把源文本同步到本地任务库：

```bash
./xrw-legacy init -source /var/lib/xrw-publisher/linuxdo-85w.txt
./xrw-legacy status
```

试跑一个图集并确认流量、频道和消息格式：

```bash
./xrw-legacy run -max 1 -workers 1
```

持续运行使用 `deploy/xrw-legacy.service`。迁移器会先下载并验证一个图集的全部原图，得到真实宽高后才按10个文件一组发布到分配频道。每组成功即保存 `file_id`、消息ID和断点并删除临时原图。源图片连续失败达到 `LEGACY_SOURCE_RETRIES` 后标为 `dead`，整包进入 `invalid`，不会进入最终D1快照。查看清理清单：

```bash
./xrw-legacy invalid-report -out /var/lib/xrw-publisher/legacy-work/invalid-albums.json
```

完成图集的完整TG映射保存在 `legacy-work/outbox/<album-id>.json`。迁移期间不修改线上D1；最终导入只合并 `ready` 图集，从而一次性替换旧Telegraph链接并排除失效图包。

GitHub Pages 不需要等待全库完成。定时快照会把已完整迁移的图集按原 ID 覆盖为带宽高的 `gimg` 地址，并把已确认失效的整包写成 `removed_ids` 移除标记；尚未处理的图集继续使用仓库中的旧 Telegraph 数据，因此迁移过程不会出现半包。手动导出命令：

```bash
./xrw-legacy snapshot -out /var/lib/xrw-publisher/github-snapshot/batches -max 100
```

`xrw-publisher-snapshot-sync` 固定把旧图库覆盖批次写入主站 `snapshot` 分支，不会混入外部 Veil 数据仓库。快照只含签名后的公开图片 URL，不含 Bot Token、原始 `file_id` 或频道私密映射。全库对账满足“ready 图集 + invalid 图集 = 14,973”后，才可停止构建时读取原始 `linuxdo-85w.txt` 数据；原文件应先归档到独立分支或 Release，不直接永久删除。

## VPS 与节点

建议使用 Linux x86_64、2 vCPU、2 GB 内存和40–60 GB SSD。发布器不会囤积全库图片，只保留当前批次、SQLite和日志；每组上传并写入断点后就删除临时原文件。单工作线程可在1 GB内存运行，但发布器和 sing-box/Xray 同机、同时开多个出口时，2 GB更稳。

发布器只接受 `socks5://` 或 `socks5h://`。VLESS、VMess、Trojan 链接交给 sing-box、Xray或mihomo，每个远端节点在本机监听一个独立SOCKS5端口，再填写：

```text
VEIL_PROXIES=socks5://127.0.0.1:10801,socks5://127.0.0.1:10802
```

轮换按这里的端口进行；只有远端出口IP不同才算不同的源站限速桶。多个本地端口最终落到同一个出口IP，不会增加可用额度。Telegram上传不走这些 Veil 代理，避免代理流量再消耗一遍。

## 完整性规则

- 图集使用稳定主键 `veil-<gallery_id>`。
- 用详情接口返回的图片列表验证 `image_id = cover_image_id + sort_order - 1`。
- 图片响应带 `x-gallery-id` 时必须与图集匹配；旧图片流不带该响应头时，以详情中的明确 image ID 为准。
- 每组文件上传成功后，在一个本地 SQLite 事务中保存该组所有单张断点，再删除临时图片。
- 图片数量、顺序和TG URL全部完整后才生成发布包。
- 新图集的每张图片必须有实际宽高，Worker 才接受发布；瀑布流会在下载前按该比例预留位置。
- Worker 用稳定图集ID去重；已是 `ok` 且源更新时间、预期数量、已发布数量都一致时，VPS直接跳过。
- 源图集改变时保留仍匹配的已上传图片，只重新处理变化部分。

若连续ID假设不成立，图集进入 `blocked`，不会误把相邻图集图片发布进去。
