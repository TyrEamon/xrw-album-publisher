# 绮影志本地上传器 · 安卓版

手机版是把**同一个 Go 上传器**装进一个 Android App 里：App 提供一个前台服务跑那个
二进制，界面就是一个 WebView 指向它自己的 `127.0.0.1:8765`，所以你看到的就是熟悉
的那套界面——站点图包、多站点切换、导入草稿、本地任务，全都一样。

```
android/                          # Gradle 工程（不含 Gradle wrapper）
├─ app/src/main/
│  ├─ java/top/mtcacg/xrw/uploader/
│  │  ├─ MainActivity.kt          # 顶栏 + WebView + 日志
│  │  └─ UploaderService.kt       # 前台服务：拉起并看护 Go 进程
│  ├─ assets/local-uploader.env   # 首次启动复制成可编辑的配置
│  ├─ jniLibs/arm64-v8a/          # 构建时生成，libxrwuploader.so
│  └─ res/                        # 图标、主题、network security config
.github/workflows/android.yml     # 云端构建 APK
```

## 为什么二进制叫 libxrwuploader.so

Android 10 起禁止执行应用数据目录里的文件，只有 `nativeLibraryDir`（也就是 APK 里
`lib/<abi>/` 解出来的那些 `.so`）可执行。所以上传器被编译成 arm64 的 ELF 后改名成
`libxrwuploader.so` 放进 `jniLibs/arm64-v8a/`，App 启动时直接 exec 它。

配套的两个开关必须同时打开，否则 `.so` 只会被内存映射、不落盘，exec 会失败：

- `app/build.gradle.kts` 里的 `packaging { jniLibs { useLegacyPackaging = true } }`
- `AndroidManifest.xml` 里的 `android:extractNativeLibs="true"`

## 构建

推送到 `main`（改动落在 `android/**`、`publisher/**` 或这个 workflow 本身）会自动跑，
也可以在 Actions 页面手动 `Run workflow`。跑完后在 Actions 那次运行的
**Artifacts** 里下载 `xrw-uploader-apk`，里面是 `app-debug.apk`。

产物是 **debug 签名** 的 APK，只能自己装，不能上应用商店。

本机有 Android SDK 也可以直接构建（需要 JDK 17 + Gradle 8.10.2）：

```bash
# 1) 先生成 arm64 二进制
cd publisher
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
  -o ../android/app/src/main/jniLibs/arm64-v8a/libxrwuploader.so ./cmd/xrw-local-uploader

# 2) 再打 APK
cd ../android
gradle assembleDebug
```

## 装到手机并配置

1. 把 `app-debug.apk` 传到手机装上（需要允许「安装未知来源应用」）。
2. 打开 App，状态栏先显示「启动中…」，几秒后变成「已就绪」，界面就出来了。
3. 点右上角 **配置**，填两个必填项：

   ```
   TG_BOT_TOKEN=123456:ABC-DEF...
   TG_CHAT_IDS=-1001234567890,-1003413999743
   ```

   其余保持默认即可：`HTTPS_PROXY=http://127.0.0.1:10808` 是手机上的代理端口，
   图和 Telegram 都要经它出去，手机上没有代理就把这两行删掉或改成实际的地址。
4. 点 **保存并重启**。想省事的话，也可以把电脑上的 `local-uploader.env` 拷到手机，
   用 **配置 → 从文件导入** 导入——里面那些 `D:\...` 路径会被 App 忽略。

第一次打开某个站点时会开始读图包列表（cosplaytele 约 2 分钟、misskon 约 10 分钟、
acgmhn 约 25 分钟），状态行会显示 `正在读取第 N/M 页图包列表`，边读边能用。

## 顶栏那几个隐藏操作

- **长按状态文字** = 重启上传器（改了配置、卡住了都按这个）
- **长按「配置」按钮** = 弹出最近的日志（启动失败时看这里）

## 手机上的数据放在哪

都在 App 的私有目录里（`/data/data/top.mtcacg.xrw.uploader/files/`）：
`data/site-albums/<站点>/index.json`、`data/telegram-imports/`、`data/batches/`、
`local-uploader.db`、`uploader.log`。**不会**写到共享存储，卸载 App 就全没了。

这几个变量由 App 直接写进子进程环境，配置文件里写也没用：
`LOCAL_UPLOADER_ADDR`、`LOCAL_UPLOADER_DATA_DIR`、`LOCAL_SNAPSHOT_DIR`、
`LOCAL_GIT_REPOSITORY=off`（**手机不推送快照分支**，快照只落在本机，要发布仍然在电脑上做）。

## 已知限制

- 上传器是**常驻前台服务**，通知栏会一直有一条「本地服务运行中」；退出 App 它还在跑。
  想彻底停掉：设置 → 应用 → 绮影志上传器 → 强行停止。
- 图片全部从源站下载再传给 Telegram，流量走手机；一次导入几十张 = 几十 MB。
- 电脑版界面里的「选文件夹」「打开快照目录」两个按钮在手机上没有对应动作，会报错；
  手机版用不到它们（草稿和快照都在私有目录，直接看界面即可）。
- 站点索引是纯文本，misskon 那份约 20 MB，读列表那段时间内存占用会高一些。
- 只出了 `arm64-v8a` 一个 ABI，2016 年之后的手机基本都支持。
