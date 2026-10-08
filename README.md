# XRW Uploader

绮影志（xrw-album）的上传器，从网站仓库里拆出来的独立工程。这里只有两样东西：

- `publisher/` —— Go 写的上传器本体。桌面版 `xrw-local-uploader`（本地网页，选文件夹上传写真）、VPS 常驻发布端 `xrw-publisher`、旧图库迁移器 `xrw-legacy`。详见 [publisher/README.md](publisher/README.md)。
- `android/` —— `xrw-local-uploader` 的安卓壳：同一个 Go 二进制装进 APK，前台服务里跑子进程，界面是 WebView 指向本机 `127.0.0.1:8765`。详见 [android/README.md](android/README.md)。

网站本身（Cloudflare Worker + GitHub Pages + 图包数据）在另一个仓库，本仓库不引用它的任何代码。

```
publisher/                        # Go 上传器，go.mod 模块根
android/                          # Gradle 工程
.github/workflows/android.yml     # 云端构建 APK
```

## 构建 APK

走 GitHub Actions：push 到 `main`（改动落在 `android/**`、`publisher/**` 或这个 workflow 本身）自动触发，也可以在 Actions 页面手动 `Run workflow`。产物是 artifact `xrw-uploader-apk` 里的 `app-debug.apk`；构建失败时会额外上传 `gradle-build-log`，里面是完整的 Gradle 输出。

CI 分两步：先把 `publisher/cmd/xrw-local-uploader` 交叉编译成 arm64 的 `libxrwuploader.so` 放进 `android/app/src/main/jniLibs/arm64-v8a/`，再跑 `gradle assembleDebug`。`jniLibs/` 是构建产物，不进 Git。

工具链版本钉在 workflow 里：AGP 8.7.3、Kotlin 2.0.21、Gradle 8.10.2。仓库不带 Gradle wrapper（它的 jar 是二进制），所以 Gradle 版本只在 workflow 的 `GRADLE_VERSION` 里改一处。本机直接构建需要自己装 JDK 17 + Android SDK。

桌面版怎么构建、怎么跑，见 [publisher/README.md](publisher/README.md)。

## 说明

这个仓库是从 `xrw-album` 复制出来的新起点，只带了 `publisher/`、`android/` 和安卓构建 workflow，没有携带原来的提交历史。
