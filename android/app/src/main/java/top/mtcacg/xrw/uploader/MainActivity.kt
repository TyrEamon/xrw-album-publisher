package top.mtcacg.xrw.uploader

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.graphics.Typeface
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.util.TypedValue
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
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
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.Executors

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

        val settings = Button(this).apply {
            text = "配置"
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 12f)
            setOnClickListener { openConfigEditor() }
            setOnLongClickListener {
                showLog()
                true
            }
        }
        bar.addView(settings, LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT))
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
        const val MAX_ATTEMPTS = 60
        const val PROBE_INTERVAL = 500L
    }
}
