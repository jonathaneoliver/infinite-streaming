package com.infinitestream.player;

import android.content.Context;
import android.content.res.Configuration;
import android.os.Build;
import android.util.DisplayMetrics;

import androidx.media3.common.MediaLibraryInfo;

// #550 Phase 4 — device / platform / version taxonomy.
//
// Android counterpart to the Swift DeviceInfo in the Apple build.
// One canonical source for the device fields we stamp on every
// session_events row. Values don't change across the process's
// lifetime so memoising on first read is safe and cheap.
//
// Mirrors the Conviva / Bitmovin breakout: collector identity
// (player_metrics_source already in use, e.g. "android") stays
// separate from hardware class (`device_class`) and physical model
// (`device_model`). `player_tech` names the playback engine so a
// future Cast / hls.js Android build can A/B against ExoPlayer on
// the same device by varying just that field.
public final class DeviceInfo {

    private DeviceInfo() { }

    /** OS major version, e.g. 14 for Android 14 (API 34). */
    public static int osVersionMajor() {
        return parseFirstInt(Build.VERSION.RELEASE, 0);
    }

    /** OS minor version, e.g. 0 for "14.0". Patch dropped at the
     *  schema level (LowCardinality-friendly cardinality). */
    public static int osVersionMinor() {
        String r = Build.VERSION.RELEASE;
        if (r == null) return 0;
        int dot = r.indexOf('.');
        if (dot < 0 || dot + 1 >= r.length()) return 0;
        return parseFirstInt(r.substring(dot + 1), 0);
    }

    /** App marketing version from the gradle build script (synced to
     *  the repo-root VERSION file). Single source of truth shared with
     *  the iOS DeviceInfo, the server image tag, and the dashboard
     *  "What's New" banner. */
    public static String appVersion() {
        return BuildConfig.VERSION_NAME == null ? "" : BuildConfig.VERSION_NAME;
    }

    /** Form-factor enum: "phone" / "tablet" / "tv". Derived from
     *  Configuration.uiMode + screen size. */
    public static String deviceClass(Context ctx) {
        if (ctx == null) return "unknown";
        int mode = ctx.getResources().getConfiguration().uiMode
            & Configuration.UI_MODE_TYPE_MASK;
        if (mode == Configuration.UI_MODE_TYPE_TELEVISION) return "tv";
        int sizeBits = ctx.getResources().getConfiguration().screenLayout
            & Configuration.SCREENLAYOUT_SIZE_MASK;
        if (sizeBits >= Configuration.SCREENLAYOUT_SIZE_LARGE) return "tablet";
        return "phone";
    }

    /** Hardware model identifier, e.g. "Pixel 7" or "AFTKA". Read
     *  from android.os.Build.MODEL. */
    public static String deviceModel() {
        return Build.MODEL == null ? "" : Build.MODEL;
    }

    /** Playback engine identifier. Hard-coded "ExoPlayer" for this
     *  Media3-based build. */
    public static String playerTech() {
        return "ExoPlayer";
    }

    /** Playback engine VERSION — the Media3/ExoPlayer library version,
     *  e.g. "1.2.1". Read at runtime from MediaLibraryInfo so it tracks
     *  the bundled dependency automatically (no hardcoding, survives a
     *  gradle bump). Crucially this is INDEPENDENT of the OS version:
     *  ExoPlayer is an app-bundled library, so two devices on the same
     *  Android release can run different player versions. (Contrast iOS,
     *  where AVPlayer IS the OS and the OS version is its version.)
     *  Pairs with player_tech for per-engine-version A/B + regression
     *  attribution. */
    public static String playerTechVersion() {
        return MediaLibraryInfo.VERSION == null ? "" : MediaLibraryInfo.VERSION;
    }

    /** Orientation-aware physical pixels of the device screen, "WxH".
     *  Mirrors iOS's `device_resolution`: changes on rotation (W and H
     *  swap) but reflects hardware capability, not the playback view's
     *  render surface. The render-surface counterpart is
     *  player_metrics_display_resolution stamped from PlayerView dims.
     *  Replaces the prior screen_width_px / screen_height_px / screen_density
     *  trio so the schema stays a single tile per concept. */
    public static String deviceResolution(Context ctx) {
        if (ctx == null) return "";
        DisplayMetrics dm = ctx.getResources().getDisplayMetrics();
        if (dm.widthPixels <= 0 || dm.heightPixels <= 0) return "";
        return dm.widthPixels + "x" + dm.heightPixels;
    }

    // Read leading run of digits as an int. Tolerates trailing
    // strings like "14.0" or "13-rc1". Falls back to defaultValue
    // if no leading digit is present.
    private static int parseFirstInt(String s, int defaultValue) {
        if (s == null) return defaultValue;
        int n = 0;
        boolean any = false;
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c < '0' || c > '9') break;
            n = n * 10 + (c - '0');
            any = true;
        }
        return any ? n : defaultValue;
    }
}
