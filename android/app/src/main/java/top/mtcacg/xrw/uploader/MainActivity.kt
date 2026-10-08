package top.mtcacg.xrw.uploader

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.BroadcastReceiver
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.graphics.Typeface
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.provider.DocumentsContract
import android.text.InputType
import android.util.TypedValue
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.Executors
import org.json.JSONObject

/**
 * Thin shell around the uploader's own web interface: it starts the background service,
 * waits for the loopback server, and then shows the page in a WebView.
 */
class MainActivity : Activity() {

    private lateinit var web: WebView
    private lateinit var stateLabel: TextView
    private lateinit var logView: TextView
    private lateinit var logScroll: ScrollView

    private val handler = Handler(Looper.getMainLooper())
    private val probePool = Executors.newSingleThreadExecutor()
    private var loaded = false
    private var probing = false

    private val stateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val text = intent?.getStringExtra(UploaderService.EXTRA_STATE) ?: return
            if (!loaded || !text.endsWith("运行中")) {
                stateLabel.text = text
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(buildLayout())
        registerStateReceiver()
        requestNotificationPermission()
        startUploaderService(UploaderService.ACTION_START)
        probe(0)
    }

    override fun onDestroy() {
        handler.removeCallbacksAndMessages(null)
        probePool.shutdownNow()
        try {
            unregisterReceiver(stateReceiver)
        } catch (_: IllegalArgumentException) {
            // Never registered.
        }
        super.onDestroy()
    }

    @Suppress("DEPRECATION")
    override fun onBackPressed() {
        if (web.canGoBack()) {
            web.goBack()
        } else {
            super.onBackPressed()
        }
    }

    @Suppress("DEPRECATION")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == REQUEST_TREE) {
            finishFolderPick(resultCode, data)
            return
        }
        if (requestCode != REQUEST_IMPORT || resultCode != RESULT_OK) {
            return
        }
        val uri: Uri = data?.data ?: return
        try {
            val text = contentResolver.openInputStream(uri)?.use { it.readBytes().toString(Charsets.UTF_8) }
            if (text.isNullOrBlank()) {
                toast("这个文件是空的")
                return
            }
            UploaderService.configFile(this).writeText(text)
            toast("已导入配置")
            restartUploader()
        } catch (error: Exception) {
            toast("导入失败：${error.message}")
        }
    }

    // ---------------------------------------------------------------- layout

    private fun buildLayout(): View {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(getColor(R.color.background))
        }

        val bar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setBackgroundColor(getColor(R.color.bar))
            setPadding(dp(14), dp(6), dp(6), dp(6))
        }
        stateLabel = TextView(this).apply {
            setTextColor(getColor(R.color.text))
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 12f)
            maxLines = 1
            text = "启动中…"
            setOnLongClickListener {
                restartUploader()
                true
            }
        }
        bar.addView(stateLabel, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))

        val configButton = Button(this).apply {
            text = "配置"
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 12f)
            setOnClickListener { openConfigEditor() }
            setOnLongClickListener {
                showLog()
                true
            }
        }
        bar.addView(configButton, LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        root.addView(bar, LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))

        val content = FrameLayout(this)
        root.addView(content, LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f))

        web = WebView(this).apply {
            setBackgroundColor(getColor(R.color.background))
            settings.javaScriptEnabled = true
            // The interface keeps its collapsed-section state in localStorage.
            settings.domStorageEnabled = true
            settings.allowFileAccess = false
            settings.allowContentAccess = false
            settings.cacheMode = WebSettings.LOAD_DEFAULT
            webViewClient = WebViewClient()
            // The page prefers these hooks over its HTTP fallbacks whenever they are
            // present, so the desktop-only folder dialog and "open directory" calls
            // never run here.
            addJavascriptInterface(HostBridge(), "XrwHost")
        }
        content.addView(web, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))

        logView = TextView(this).apply {
            setTextColor(getColor(R.color.text_dim))
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 11f)
            typeface = Typeface.MONOSPACE
            setPadding(dp(14), dp(14), dp(14), dp(14))
        }
        logScroll = ScrollView(this).apply {
            setBackgroundColor(getColor(R.color.background))
            visibility = View.GONE
            addView(logView)
        }
        content.addView(logScroll, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))

        return root
    }

    // --------------------------------------------------------------- startup

    private fun probe(attempt: Int) {
        if (loaded || probing) {
            return
        }
        probing = true
        if (attempt == 0) {
            stateLabel.text = "启动中…"
        }
        probePool.execute {
            val ready = stateEndpointAnswers()
            handler.post {
                probing = false
                when {
                    ready -> showPage()
                    attempt >= MAX_ATTEMPTS -> {
                        stateLabel.text = "启动失败，长按配置看日志"
                        showLog()
                    }

                    else -> handler.postDelayed({ probe(attempt + 1) }, PROBE_INTERVAL)
                }
            }
        }
    }

    private fun stateEndpointAnswers(): Boolean {
        return try {
            val connection = URL(UploaderService.BASE_URL + "api/state").openConnection() as HttpURLConnection
            connection.connectTimeout = 900
            connection.readTimeout = 900
            connection.requestMethod = "GET"
            val code = connection.responseCode
            connection.disconnect()
            code in 200..299
        } catch (_: Exception) {
            false
        }
    }

    private fun showPage() {
        loaded = true
        stateLabel.text = "已就绪"
        logScroll.visibility = View.GONE
        if (web.url == null) {
            web.loadUrl(UploaderService.BASE_URL)
        } else {
            web.reload()
        }
    }

    private fun restartUploader() {
        loaded = false
        probing = false
        logScroll.visibility = View.GONE
        stateLabel.text = "正在重启…"
        startUploaderService(UploaderService.ACTION_RESTART)
        handler.postDelayed({ probe(0) }, 700)
    }

    private fun startUploaderService(action: String) {
        startForegroundService(Intent(this, UploaderService::class.java).setAction(action))
    }

    private fun showLog() {
        val file = UploaderService.logFile(this)
        val text = if (file.exists()) file.readText().takeLast(6000) else ""
        logView.text = text.ifBlank { "还没有日志。" }
        logScroll.visibility = View.VISIBLE
    }

    // ---------------------------------------------------------------- config

    private fun openConfigEditor() {
        val file = UploaderService.configFile(this)
        val editor = EditText(this).apply {
            setText(if (file.exists()) file.readText() else "")
            typeface = Typeface.MONOSPACE
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 12f)
            inputType = InputType.TYPE_CLASS_TEXT or
                InputType.TYPE_TEXT_FLAG_MULTI_LINE or
                InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            setPadding(dp(16), dp(16), dp(16), dp(16))
        }
        val scroll = ScrollView(this).apply { addView(editor) }
        AlertDialog.Builder(this)
            .setTitle("上传器配置")
            .setView(scroll)
            .setPositiveButton("保存并重启") { _, _ ->
                file.writeText(editor.text.toString())
                toast("配置已保存")
                restartUploader()
            }
            .setNeutralButton("从文件导入") { _, _ -> pickConfigFile() }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun pickConfigFile() {
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            type = "*/*"
        }
        startActivityForResult(intent, REQUEST_IMPORT)
    }

    // ----------------------------------------------------------- folder pick

    /**
     * Hooks the page inside the WebView calls instead of the desktop HTTP fallbacks:
     * Android has no folder dialog, no file manager that can open a private app
     * directory, and no clipboard the page itself may write to.
     */
    private inner class HostBridge {
        @JavascriptInterface
        fun pickFolder() {
            runOnUiThread { startFolderPick() }
        }

        @JavascriptInterface
        fun describeFolder(path: String): String {
            val directory = File(path)
            var files = 0
            var bytes = 0L
            if (directory.isDirectory) {
                directory.walkTopDown().take(MAX_DESCRIBED_FILES).forEach { entry ->
                    if (entry.isFile) {
                        files += 1
                        bytes += entry.length()
                    }
                }
            }
            return JSONObject()
                .put("exists", directory.isDirectory)
                .put("files", files)
                .put("bytes", bytes)
                .toString()
        }

        @JavascriptInterface
        fun copyText(text: String) {
            runOnUiThread {
                getSystemService(ClipboardManager::class.java)?.setPrimaryClip(ClipData.newPlainText("绮影志", text))
                toast("路径已复制")
            }
        }

        @JavascriptInterface
        fun showToast(message: String) {
            runOnUiThread { toast(message) }
        }
    }

    private fun startFolderPick() {
        val permission = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            Manifest.permission.READ_MEDIA_IMAGES
        } else {
            Manifest.permission.READ_EXTERNAL_STORAGE
        }
        if (checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED) {
            launchTreePicker()
            return
        }
        // Ask first: reading the picked folder happens in the uploader process, which
        // needs the permission granted to the app, not just the picker's URI grant.
        requestPermissions(arrayOf(permission), REQUEST_MEDIA)
    }

    private fun launchTreePicker() {
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT_TREE).apply {
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION)
        }
        try {
            startActivityForResult(intent, REQUEST_TREE)
        } catch (error: Exception) {
            answerFolderPick(null, "打不开系统文件夹选择器：${error.message}")
        }
    }

    private fun finishFolderPick(resultCode: Int, data: Intent?) {
        val tree = if (resultCode == RESULT_OK) data?.data else null
        if (tree == null) {
            answerFolderPick(null, "没有选择文件夹")
            return
        }
        try {
            contentResolver.takePersistableUriPermission(tree, Intent.FLAG_GRANT_READ_URI_PERMISSION)
        } catch (_: SecurityException) {
            // Not every provider hands out a persistable grant; the copy below still works.
        }
        val direct = readablePath(tree)
        if (direct != null) {
            answerFolderPick(direct, null)
            return
        }
        // Providers outside shared storage (and volumes the app cannot read) are copied
        // into app storage, which the uploader can always read.
        toast("正在把选中的目录复制到应用空间…")
        val copied = copyTree(tree)
        if (copied != null) {
            answerFolderPick(copied, null)
        } else {
            answerFolderPick(null, "这个位置读不到图片，请改选「内部存储」里的目录")
        }
    }

    private fun answerFolderPick(path: String?, message: String?) {
        val payload = JSONObject()
        if (path != null) payload.put("path", path)
        if (message != null) payload.put("message", message)
        val script = "window.XrwHostResult && window.XrwHostResult($payload)"
        runOnUiThread { web.evaluateJavascript(script, null) }
    }

    /** Maps a picked tree onto a real path when the volume exposes one. */
    private fun readablePath(tree: Uri): String? {
        if (tree.authority != "com.android.externalstorage.documents") {
            return null
        }
        val document = runCatching { DocumentsContract.getTreeDocumentId(tree) }.getOrNull() ?: return null
        val parts = document.split(":", limit = 2)
        if (parts.size != 2) {
            return null
        }
        val root = if (parts[0].equals("primary", ignoreCase = true)) {
            Environment.getExternalStorageDirectory().absolutePath
        } else {
            "/storage/${parts[0]}"
        }
        val path = if (parts[1].isEmpty()) root else "$root/${parts[1]}"
        val entries = File(path).listFiles() ?: return null
        // A directory that lists but whose files stay unreadable is no use to the
        // uploader, so let the copy fallback take it.
        return if (entries.any { it.isFile && !it.canRead() }) null else path
    }

    /** Copies a picked tree into app storage, one folder per pick. */
    private fun copyTree(tree: Uri): String? {
        val name = DocumentsContract.getTreeDocumentId(tree).substringAfter(':').substringAfterLast('/', "")
        val target = File(File(filesDir, "imports"), "${name.ifBlank { "picked" }}-${System.currentTimeMillis()}")
        if (!target.mkdirs()) {
            return null
        }
        var copied = 0
        val pending = ArrayDeque<Pair<String, File>>()
        pending.addLast(DocumentsContract.getTreeDocumentId(tree) to target)
        while (pending.isNotEmpty() && copied < MAX_COPIED_FILES) {
            val (document, directory) = pending.removeLast()
            val children = DocumentsContract.buildChildDocumentsUriUsingTree(tree, document)
            contentResolver.query(
                children,
                arrayOf(
                    DocumentsContract.Document.COLUMN_DOCUMENT_ID,
                    DocumentsContract.Document.COLUMN_DISPLAY_NAME,
                    DocumentsContract.Document.COLUMN_MIME_TYPE,
                ),
                null,
                null,
                null,
            )?.use { cursor ->
                while (cursor.moveToNext()) {
                    val child = cursor.getString(0)
                    val display = cursor.getString(1) ?: continue
                    if (cursor.getString(2) == DocumentsContract.Document.MIME_TYPE_DIR) {
                        val nested = File(directory, display)
                        if (nested.mkdirs()) {
                            pending.addLast(child to nested)
                        }
                        continue
                    }
                    if (IMAGE_EXTENSIONS.none { display.lowercase().endsWith(it) }) {
                        continue
                    }
                    val file = File(directory, display)
                    try {
                        val source = contentResolver.openInputStream(DocumentsContract.buildDocumentUriUsingTree(tree, child))
                        source?.use { input -> file.outputStream().use { output -> input.copyTo(output) } }
                        copied += 1
                    } catch (_: Exception) {
                        file.delete()
                    }
                }
            }
        }
        return if (copied > 0) target.absolutePath else null
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == REQUEST_MEDIA) {
            // The picker is useful either way: without the grant the copy fallback runs.
            launchTreePicker()
        }
    }

    // ------------------------------------------------------------- amenities

    private fun registerStateReceiver() {
        val filter = IntentFilter(UploaderService.ACTION_STATE)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            registerReceiver(stateReceiver, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            registerReceiver(stateReceiver, filter)
        }
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            return
        }
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_NOTIFICATIONS)
        }
    }

    private fun toast(message: String) {
        Toast.makeText(this, message, Toast.LENGTH_SHORT).show()
    }

    private fun dp(value: Int): Int = Math.round(value * resources.displayMetrics.density)

    private companion object {
        const val REQUEST_NOTIFICATIONS = 1
        const val REQUEST_IMPORT = 2
        const val REQUEST_TREE = 3
        const val REQUEST_MEDIA = 4
        const val MAX_ATTEMPTS = 60
        const val PROBE_INTERVAL = 500L
        const val MAX_DESCRIBED_FILES = 20000
        const val MAX_COPIED_FILES = 5000
        val IMAGE_EXTENSIONS = listOf(".jpg", ".jpeg", ".png", ".gif", ".webp")
    }
}
