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
        // CI stamps every build so the tablet can show which one it carries.
        // A build on a developer machine keeps 1, which still installs over
        // itself.
        versionCode = System.getenv("APK_VERSION_CODE")?.toIntOrNull() ?: 1
        versionName = System.getenv("APK_VERSION_NAME") ?: "1.0.0-dev"
    }

    /*
        Android refuses to install an APK over one signed with a different
        key, and the fresh runner that builds each release has no keystore of
        its own, so Gradle would invent a throwaway one every time. That is
        what forces an uninstall, and an uninstall costs the tablet its saved
        configuration. CI decodes a keystore from a repository secret into
        release.p12 instead, so every build carries the same signature.

        A build with no keystore present falls back to Gradle's local debug
        key, which is all a developer machine needs.
    */
    val sharedKeystore = rootProject.file("release.p12")
    val sharedKeystorePassword: String? = System.getenv("ANDROID_KEYSTORE_PASSWORD")
    val signWithSharedKey = sharedKeystore.isFile && !sharedKeystorePassword.isNullOrEmpty()

    signingConfigs {
        if (signWithSharedKey) {
            create("shared") {
                storeFile = sharedKeystore
                storeType = "PKCS12"
                storePassword = sharedKeystorePassword
                // A PKCS12 keystore protects its key with the store password.
                keyAlias = System.getenv("ANDROID_KEY_ALIAS") ?: "streaming"
                keyPassword = sharedKeystorePassword
            }
        }
    }

    buildTypes {
        debug {
            if (signWithSharedKey) {
                signingConfig = signingConfigs.getByName("shared")
            }
        }
        release {
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            if (signWithSharedKey) {
                signingConfig = signingConfigs.getByName("shared")
            }
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

    sourceSets {
        getByName("main") {
            jniLibs.srcDir(layout.buildDirectory.dir("generated/jniLibs"))
        }
    }

    packaging {
        jniLibs {
            // The Go server is an executable named like a native library so
            // Android extracts it to the app's executable nativeLibraryDir.
            useLegacyPackaging = true
            keepDebugSymbols += "**/libstreaming.so"
        }
    }
}

val buildAndroidGoServer by tasks.registering(Exec::class) {
    group = "build"
	description = "Cross-compiles the Go control server for Android ARM64"
    workingDir(rootProject.projectDir)
    commandLine("bash", rootProject.file("scripts/build-android-go.sh").absolutePath)

	inputs.files(
		rootProject.fileTree(".") {
			include("*.go", "go.mod", "go.sum")
			include("internal/**/*.go", "web/**", "scripts/build-android-go.sh")
			exclude("app/**", "build/**")
		},
	)
    outputs.files(
        layout.buildDirectory.file("generated/jniLibs/arm64-v8a/libstreaming.so"),
    )
}

tasks.named("preBuild").configure {
    dependsOn(buildAndroidGoServer)
}

dependencies {
    implementation("androidx.core:core-ktx:1.12.0")
    implementation("androidx.appcompat:appcompat:1.6.1")
    implementation("com.google.android.material:material:1.11.0")
    implementation("androidx.constraintlayout:constraintlayout:2.1.4")
    implementation("androidx.security:security-crypto:1.1.0-alpha06")
    implementation("androidx.work:work-runtime-ktx:2.9.0")
}
