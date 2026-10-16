plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

android {
    namespace = "io.p2psync.android"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.p2psync.android"
        minSdk = 26
        targetSdk = 36
        versionCode = 2
        versionName = "0.4.0"
        // The engine ships native code for 64-bit ARM phones only (see scripts/build-aar.sh).
        ndk { abiFilters += listOf("arm64-v8a") }
    }

    // No signing key lives in the repository. A release build is signed when the
    // keystore is given through the environment (CI secrets, or your own shell).
    val keystore = System.getenv("P2PSYNC_KEYSTORE")
    signingConfigs {
        if (keystore != null) {
            create("release") {
                storeFile = file(keystore)
                storePassword = System.getenv("P2PSYNC_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("P2PSYNC_KEY_ALIAS")
                keyPassword = System.getenv("P2PSYNC_KEY_PASSWORD") ?: System.getenv("P2PSYNC_KEYSTORE_PASSWORD")
            }
        }
    }
    buildTypes {
        release {
            isMinifyEnabled = false // the Go engine is native; nothing useful to shrink
            if (keystore != null) signingConfig = signingConfigs.getByName("release")
        }
    }
    // Do not embed Google's encrypted dependency metadata blob in the APK.
    dependenciesInfo {
        includeInApk = false
        includeInBundle = false
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }
    buildFeatures { compose = true }
    packaging {
        jniLibs { useLegacyPackaging = false }
        resources.excludes += "/META-INF/{AL2.0,LGPL2.1}"
    }
    lint {
        abortOnError = true
        warningsAsErrors = false
    }
    testOptions { unitTests.isReturnDefaultValues = true }
}

dependencies {
    // Go engine built by scripts/build-aar.sh
    implementation(files("libs/p2psync.aar"))

    implementation("androidx.core:core-ktx:1.17.0")
    implementation("androidx.activity:activity-compose:1.11.0")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.9.4")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.9.4")

    val bom = platform("androidx.compose:compose-bom:2025.10.00")
    implementation(bom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-core")
    implementation("androidx.compose.ui:ui-tooling-preview")
    debugImplementation("androidx.compose.ui:ui-tooling")

    // QR scanning: CameraX for frames, ZXing (open source, no Google services) to decode/encode.
    val camerax = "1.5.1"
    implementation("androidx.camera:camera-core:$camerax")
    implementation("androidx.camera:camera-camera2:$camerax")
    implementation("androidx.camera:camera-lifecycle:$camerax")
    implementation("androidx.camera:camera-view:$camerax")
    implementation("com.google.zxing:core:3.5.3")

    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")

    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20250517")
}
