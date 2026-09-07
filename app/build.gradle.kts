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
            include("internal/**/*.go", "web/**")
            exclude("app/**", "build/**")
        },
    )
    outputs.file(layout.buildDirectory.file("generated/jniLibs/arm64-v8a/libstreaming.so"))
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
}
