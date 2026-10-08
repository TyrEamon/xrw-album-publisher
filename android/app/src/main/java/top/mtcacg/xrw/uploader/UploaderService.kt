package top.mtcacg.xrw.uploader

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import java.io.BufferedReader
import java.io.File
import java.io.FileOutputStream
import java.io.InputStreamReader
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

/**
 * Runs the Go uploader as a child process and keeps it alive.
 *
 * The uploader is a linux/arm64 ELF shipped as libxrwuploader.so: only files inside the
 * native library directory may be executed on modern Android, so assets or filesDir cannot
 * be used directly. See android/README.md.
 */
class UploaderService : Service() {

    private val handler = Handler(Looper.getMainLooper())
    private var child: Process? = null
    private var restarts = 0
    private var wanted = false

    private val watch = object : Runnable {
        override fun run() {
            if (!wanted) {
                return
            }
            val running = child
            if (running != null && running.isAlive) {
                restarts = 0
            } else {
                restarts += 1
                if (restarts <= MAX_RESTARTS) {
                    publish("上传器退出，正在重启（第 $restarts 次）")
                    launch()
                } else {
                    publish("上传器反复退出，长按配置查看日志")
                }
            }
            handler.postDelayed(this, WATCH_INTERVAL)
        }
    }

    override fun onCreate() {
        super.onCreate()
        createChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startInForeground()
        wanted = true
        if (intent?.action == ACTION_RESTART) {
            publish("正在重启…")
            stopChild()
            restarts = 0
        }
        launch()
        handler.removeCallbacks(watch)
        handler.postDelayed(watch, WATCH_INTERVAL)
        return START_STICKY
    }

    override fun onDestroy() {
        wanted = false
        handler.removeCallbacks(watch)
        stopChild()
        stopForeground(STOP_FOREGROUND_REMOVE)
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun launch() {
        val running = child
        if (running != null && running.isAlive) {
            publish("本地服务运行中")
            return
        }
        val config = prepareConfig()
        val binary = File(applicationInfo.nativeLibraryDir, BINARY_NAME)
        if (!binary.exists()) {
            publish("这个 APK 里没有上传器内核，请重新安装")
            return
        }
        try {
            val builder = ProcessBuilder(binary.absolutePath, "-config", config.absolutePath, "-no-browser")
            builder.directory(filesDir)
            builder.redirectErrorStream(true)
            // These keys win over the config file: loadEnvFile only fills missing variables,
            // so a desktop env file copied onto the phone cannot aim the data directory at D:\.
            val environment = builder.environment()
            environment["LOCAL_UPLOADER_ADDR"] = ADDRESS
            environment["LOCAL_UPLOADER_DATA_DIR"] = dataDir(this).absolutePath
            environment["LOCAL_SNAPSHOT_DIR"] = snapshotDir(this).absolutePath
            environment["LOCAL_GIT_REPOSITORY"] = "off"
            environment["HOME"] = filesDir.absolutePath
            environment["TMPDIR"] = cacheDir.absolutePath
            val started = builder.start()
            child = started
            pump(started)
            publish("本地服务运行中")
        } catch (error: Exception) {
            publish("启动失败：${error.message}")
        }
    }

    /** Drains the child's merged stdout/stderr into filesDir/uploader.log. */
    private fun pump(process: Process) {
        val sink = logFile(this)
        if (sink.length() > LOG_LIMIT) {
            sink.delete()
        }
        thread(name = "uploader-log", isDaemon = true) {
            try {
                FileOutputStream(sink, true).use { output ->
                    output.write("---- 启动于 ${stamp()} ----\n".toByteArray())
                    output.flush()
                    BufferedReader(InputStreamReader(process.inputStream)).useLines { lines ->
                        for (line in lines) {
                            output.write((line + "\n").toByteArray())
                            output.flush()
                        }
                    }
                }
            } catch (_: Exception) {
                // The child is gone; the watchdog decides what happens next.
            }
        }
    }

    private fun stopChild() {
        val running = child ?: return
        child = null
        running.destroy()
        try {
            if (!running.waitFor(3, TimeUnit.SECONDS)) {
                running.destroyForcibly()
            }
        } catch (_: InterruptedException) {
            running.destroyForcibly()
        }
    }

    private fun prepareConfig(): File {
        val target = configFile(this)
        if (!target.exists()) {
            assets.open(ENV_NAME).use { input ->
                target.outputStream().use { output -> input.copyTo(output) }
            }
        }
        return target
    }

    private fun publish(text: String) {
        sendBroadcast(Intent(ACTION_STATE).setPackage(packageName).putExtra(EXTRA_STATE, text))
    }

    private fun startInForeground() {
        val notification = buildNotification()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    private fun buildNotification(): Notification {
        val open = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(getString(R.string.notification_text))
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setOngoing(true)
            .setContentIntent(open)
            .build()
    }

    private fun createChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        if (manager.getNotificationChannel(CHANNEL_ID) == null) {
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, getString(R.string.notification_channel), NotificationManager.IMPORTANCE_LOW),
            )
        }
    }

    private fun stamp(): String = SimpleDateFormat("MM-dd HH:mm:ss", Locale.US).format(Date())

    companion object {
        const val ACTION_START = "top.mtcacg.xrw.uploader.START"
        const val ACTION_RESTART = "top.mtcacg.xrw.uploader.RESTART"
        const val ACTION_STATE = "top.mtcacg.xrw.uploader.STATE"
        const val EXTRA_STATE = "state"

        const val PORT = 8765
        const val ADDRESS = "127.0.0.1:$PORT"
        const val BASE_URL = "http://$ADDRESS/"

        private const val CHANNEL_ID = "uploader"
        private const val NOTIFICATION_ID = 1
        private const val BINARY_NAME = "libxrwuploader.so"
        private const val ENV_NAME = "local-uploader.env"
        private const val LOG_NAME = "uploader.log"
        private const val LOG_LIMIT = 2L * 1024L * 1024L
        private const val WATCH_INTERVAL = 5_000L
        private const val MAX_RESTARTS = 5

        fun configFile(context: Context): File = File(context.filesDir, ENV_NAME)

        fun logFile(context: Context): File = File(context.filesDir, LOG_NAME)

        fun dataDir(context: Context): File = File(context.filesDir, "data").apply { mkdirs() }

        fun snapshotDir(context: Context): File = File(dataDir(context), "batches").apply { mkdirs() }
    }
}
