plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "top.mtcacg.xrw.uploader"
    compileSdk = 35

    defaultConfig {
        applicationId = "top.mtcacg.xrw.uploader"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    // The uploader is shipped as libxrwuploader.so because the installer extracts
    // native libraries with the executable bit set. Android refuses to exec files
    // that live inside the app data directory, so assets/filesDir are not an option.
    packaging {
        jniLibs {
            useLegacyPackaging = true
        }
    }

    buildTypes {
        // Sideloaded only, so the debug key is fine and no keystore has to be kept.
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }
}
