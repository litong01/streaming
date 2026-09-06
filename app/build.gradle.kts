plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.streaming.app"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.streaming.app"
        minSdk = 26
        targetSdk = 34
        versionCode = 1
        versionName = "1.0.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        viewBinding = true
    }

    // The served pages live in the repository's web directory, shared with the
    // standalone Go server, and ship in the APK as assets.
    sourceSets {
        getByName("main") {
            assets.srcDir(rootProject.file("web"))
        }
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.12.0")
    implementation("androidx.appcompat:appcompat:1.6.1")
    implementation("com.google.android.material:material:1.11.0")
    implementation("androidx.constraintlayout:constraintlayout:2.1.4")
    implementation("androidx.security:security-crypto:1.1.0-alpha06")
    implementation("org.nanohttpd:nanohttpd:2.3.1")
    implementation("com.github.mwiede:jsch:0.2.16")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.7.3")
}
