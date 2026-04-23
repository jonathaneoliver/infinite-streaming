package com.infinitestream.player;

import android.content.Context;
import android.content.res.ColorStateList;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import androidx.constraintlayout.widget.ConstraintLayout;

import androidx.annotation.OptIn;
import androidx.appcompat.app.AlertDialog;
import androidx.appcompat.app.AppCompatActivity;
import androidx.media3.common.C;
import androidx.media3.common.Format;
import androidx.media3.common.MediaItem;
import androidx.media3.common.Player;
import androidx.media3.common.util.UnstableApi;
import androidx.media3.exoplayer.DecoderReuseEvaluation;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.exoplayer.analytics.AnalyticsListener;
import androidx.media3.exoplayer.source.LoadEventInfo;
import androidx.media3.exoplayer.source.MediaLoadData;
import androidx.media3.exoplayer.upstream.DefaultBandwidthMeter;
import androidx.media3.exoplayer.video.VideoFrameMetadataListener;
import androidx.media3.ui.PlayerView;

import com.google.android.material.button.MaterialButton;
import com.google.android.material.button.MaterialButtonToggleGroup;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

@OptIn(markerClass = UnstableApi.class)
public class MainActivity extends AppCompatActivity {

    private ExoPlayer player;
    private PlayerView playerView;
    private MaterialButton retryButton;
    private MaterialButton restartButton;
    private MaterialButton reloadButton;
    private MaterialButtonToggleGroup serverToggle;
    private MaterialButtonToggleGroup protocolToggle;
    private MaterialButtonToggleGroup segmentToggle;
    private MaterialButtonToggleGroup codecToggle;
    private MaterialButton contentButton;
    private TextView statusText;
    private MaterialButton fullscreenButton;
    private View rightPanel;
    private View panelGuideline;
    private boolean isFullscreen = false;

    private String currentUrl = "";
    private final String playerId = UUID.randomUUID().toString();
    private PlaybackMetrics metrics;

    private static class ServerEnvironment {
        final String name;
        final String host;
        final String port;
        final String apiPort;

        ServerEnvironment(String name, String host, String port, String apiPort) {
            this.name = name;
            this.host = host;
            this.port = port;
            this.apiPort = apiPort;
        }

        String label() {
            return name;
        }
    }

    private final ServerEnvironment[] servers = {
        new ServerEnvironment("Ubuntu", "192.168.0.106", "21081", "21000"),
        new ServerEnvironment("Dev", "100.111.190.54", "40081", "40000"),
        new ServerEnvironment("Release", "infinitestreaming.jeoliver.com", "30081", "30000")
    };

    private final String[] protocols = {"HLS", "DASH"};
    private final String[] segments = {"LL", "2s", "6s"};
    private final String[] codecs = {"Auto", "H264", "H265/HEVC", "AV1"};

    private static final int DEFAULT_SEGMENT_INDEX = 2; // 6s

    private final List<ContentItem> availableContent = new ArrayList<>();
    private String selectedContent = "";
    private final ExecutorService networkExecutor = Executors.newSingleThreadExecutor();
    private final Handler mainHandler = new Handler(Looper.getMainLooper());

    private static class ContentItem {
        final String name;
        final boolean hasHls;
        final boolean hasDash;

        ContentItem(String name, boolean hasHls, boolean hasDash) {
            this.name = name;
            this.hasHls = hasHls;
            this.hasDash = hasDash;
        }
    }

    private static String inferCodec(String name) {
        String lower = name.toLowerCase(Locale.US);
        if (lower.contains("av1")) return "AV1";
        if (lower.contains("hevc") || lower.contains("h265")) return "H265/HEVC";
        return "H264";
    }

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        playerView = findViewById(R.id.player_view);
        retryButton = findViewById(R.id.retry_button);
        restartButton = findViewById(R.id.restart_button);
        reloadButton = findViewById(R.id.reload_button);
        serverToggle = findViewById(R.id.server_toggle);
        protocolToggle = findViewById(R.id.protocol_toggle);
        segmentToggle = findViewById(R.id.segment_toggle);
        codecToggle = findViewById(R.id.codec_toggle);
        contentButton = findViewById(R.id.content_button);
        statusText = findViewById(R.id.status_text);
        fullscreenButton = findViewById(R.id.fullscreen_button);
        rightPanel = findViewById(R.id.right_panel);
        panelGuideline = findViewById(R.id.panel_guideline);

        fullscreenButton.setOnClickListener(v -> toggleFullscreen());

        styleActionButton(retryButton);
        styleActionButton(restartButton);
        styleActionButton(reloadButton);
        styleActionButton(contentButton);
        styleActionButton(fullscreenButton);

        initializePlayer();
        retryButton.requestFocus(); // start focus top-left of left panel
        setupToggles();

        retryButton.setOnClickListener(v -> retryFetch());
        restartButton.setOnClickListener(v -> restartPlayback());
        reloadButton.setOnClickListener(v -> {
            fetchContentList();
        });
        contentButton.setOnClickListener(v -> showContentPicker());
    }

    private void initializePlayer() {
        DefaultBandwidthMeter bandwidthMeter = new DefaultBandwidthMeter.Builder(this).build();
        player = new ExoPlayer.Builder(this)
            .setBandwidthMeter(bandwidthMeter)
            .build();
        playerView.setPlayer(player);

        metrics = new PlaybackMetrics(
            player,
            playerView,
            bandwidthMeter,
            playerId,
            () -> {
                ServerEnvironment s = currentServer();
                return "http://" + s.host + ":" + s.port;
            },
            () -> currentUrl
        );

        player.addListener(new Player.Listener() {
            @Override
            public void onPlayerError(androidx.media3.common.PlaybackException error) {
                statusText.setText("Error: " + error.getMessage());
                Toast.makeText(MainActivity.this, "Playback error: " + error.getMessage(),
                    Toast.LENGTH_LONG).show();
                metrics.onPlayerError(error.getMessage());
            }

            @Override
            public void onRenderedFirstFrame() {
                metrics.onFirstFrameRendered();
            }

            @Override
            public void onIsPlayingChanged(boolean isPlaying) {
                if (isPlaying) {
                    getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                    metrics.onStallEnd();
                } else {
                    getWindow().clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                }
            }

            @Override
            public void onPlaybackStateChanged(int state) {
                if (state == Player.STATE_BUFFERING
                    && player.getPlayWhenReady()
                    && !player.isPlaying()) {
                    metrics.onStallStart();
                }
            }
        });

        player.addAnalyticsListener(new AnalyticsListener() {
            @Override
            public void onDroppedVideoFrames(EventTime eventTime, int droppedFrames,
                                              long elapsedMs) {
                metrics.onDroppedFrames(droppedFrames);
            }

            @Override
            public void onVideoInputFormatChanged(EventTime eventTime, Format format,
                                                   DecoderReuseEvaluation decoderReuseEvaluation) {
                metrics.onVideoFormatChanged(format);
            }

            @Override
            public void onLoadCompleted(EventTime eventTime,
                                        LoadEventInfo loadEventInfo,
                                        MediaLoadData mediaLoadData) {
                if (mediaLoadData.trackType == C.TRACK_TYPE_VIDEO) {
                    metrics.onVideoLoadCompleted();
                }
            }
        });

        player.setVideoFrameMetadataListener(new VideoFrameMetadataListener() {
            @Override
            public void onVideoFrameAboutToBeRendered(long presentationTimeUs,
                                                      long releaseTimeNs,
                                                      Format format,
                                                      android.media.MediaFormat mediaFormat) {
                metrics.onFrameRendered();
            }
        });
    }

    private void setupToggles() {
        for (int i = 0; i < servers.length; i++) {
            addToggleButton(serverToggle, i, servers[i].label());
        }
        for (int i = 0; i < protocols.length; i++) {
            addToggleButton(protocolToggle, i, protocols[i]);
        }
        for (int i = 0; i < segments.length; i++) {
            addToggleButton(segmentToggle, i, segments[i]);
        }
        for (int i = 0; i < codecs.length; i++) {
            addToggleButton(codecToggle, i, codecs[i]);
        }

        // Only the leftmost button in each group escapes to the left panel
        for (MaterialButtonToggleGroup group :
                new MaterialButtonToggleGroup[]{ serverToggle, protocolToggle, segmentToggle, codecToggle }) {
            if (group.getChildCount() > 0) {
                group.getChildAt(0).setNextFocusLeftId(R.id.fullscreen_button);
            }
        }

        serverToggle.check(serverToggle.getChildAt(0).getId());
        protocolToggle.check(protocolToggle.getChildAt(0).getId());
        segmentToggle.check(segmentToggle.getChildAt(DEFAULT_SEGMENT_INDEX).getId());
        codecToggle.check(codecToggle.getChildAt(0).getId());

        serverToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) fetchContentList();
        });
        protocolToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) applyContentFilter();
        });
        segmentToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) buildUrlAndLoad();
        });
        codecToggle.addOnButtonCheckedListener((group, checkedId, isChecked) -> {
            if (isChecked) applyContentFilter();
        });

        fetchContentList();
    }

    private void styleActionButton(MaterialButton button) {
        int[][] states = {
            { android.R.attr.state_focused },
            { -android.R.attr.state_focused }
        };
        button.setTextColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFFFFFFFF }));
        button.setBackgroundTintList(new ColorStateList(states,
            new int[]{ 0xFF1A3A4A, 0x00000000 }));
        button.setStrokeColor(new ColorStateList(states,
            new int[]{ 0xFF00B4D8, 0xFF555555 }));
    }

    private void addToggleButton(MaterialButtonToggleGroup group, int index, String text) {
        MaterialButton button = new MaterialButton(
            new androidx.appcompat.view.ContextThemeWrapper(this,
                com.google.android.material.R.style.Widget_MaterialComponents_Button_OutlinedButton),
            null, com.google.android.material.R.attr.materialButtonOutlinedStyle);
        button.setId(View.generateViewId());
        button.setText(text);
        button.setCheckable(true);
        button.setTextSize(android.util.TypedValue.COMPLEX_UNIT_SP, 12);

        int[][] states = {
            { android.R.attr.state_focused,  android.R.attr.state_checked },
            {                                android.R.attr.state_checked },
            { android.R.attr.state_focused, -android.R.attr.state_checked },
            {                               -android.R.attr.state_checked }
        };
        button.setTextColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF, 0xFF888888 }));
        button.setBackgroundTintList(new ColorStateList(states,
            new int[]{ 0xFF00B4D8, 0xFF0077B6, 0xFF1A3A4A, 0x00000000 }));
        button.setStrokeColor(new ColorStateList(states,
            new int[]{ 0xFFFFFFFF, 0xFF00B4D8, 0xFF00B4D8, 0xFF444444 }));

        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
            ViewGroup.LayoutParams.WRAP_CONTENT,
            ViewGroup.LayoutParams.WRAP_CONTENT);
        button.setLayoutParams(lp);
        group.addView(button);
    }

    private int selectedIndex(MaterialButtonToggleGroup group) {
        int id = group.getCheckedButtonId();
        for (int i = 0; i < group.getChildCount(); i++) {
            if (group.getChildAt(i).getId() == id) return i;
        }
        return -1;
    }

    private ServerEnvironment currentServer() {
        int i = selectedIndex(serverToggle);
        return i >= 0 ? servers[i] : servers[0];
    }

    private String currentProtocol() {
        int i = selectedIndex(protocolToggle);
        return i >= 0 ? protocols[i] : protocols[0];
    }

    private String currentSegment() {
        int i = selectedIndex(segmentToggle);
        return i >= 0 ? segments[i] : segments[DEFAULT_SEGMENT_INDEX];
    }

    private String currentCodec() {
        int i = selectedIndex(codecToggle);
        return i >= 0 ? codecs[i] : codecs[0];
    }

    private void fetchContentList() {
        ServerEnvironment server = currentServer();
        final String urlStr = String.format(Locale.US, "http://%s:%s/api/content",
            server.host, server.apiPort);
        statusText.setText("Loading content list…");
        networkExecutor.execute(() -> {
            List<ContentItem> fetched = new ArrayList<>();
            String error = null;
            HttpURLConnection conn = null;
            try {
                URL url = new URL(urlStr);
                conn = (HttpURLConnection) url.openConnection();
                conn.setConnectTimeout(5000);
                conn.setReadTimeout(5000);
                int code = conn.getResponseCode();
                if (code != 200) {
                    error = "HTTP " + code + " from " + urlStr;
                } else {
                    StringBuilder sb = new StringBuilder();
                    try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                        String line;
                        while ((line = r.readLine()) != null) sb.append(line);
                    }
                    JSONArray arr = new JSONArray(sb.toString());
                    for (int i = 0; i < arr.length(); i++) {
                        JSONObject o = arr.getJSONObject(i);
                        fetched.add(new ContentItem(
                            o.getString("name"),
                            o.optBoolean("has_hls", false),
                            o.optBoolean("has_dash", false)));
                    }
                }
            } catch (Exception e) {
                error = e.getClass().getSimpleName() + ": " + e.getMessage();
            } finally {
                if (conn != null) conn.disconnect();
            }
            final String errMsg = error;
            mainHandler.post(() -> {
                if (errMsg != null) {
                    statusText.setText("Fetch failed: " + errMsg);
                    Toast.makeText(MainActivity.this, errMsg, Toast.LENGTH_LONG).show();
                    availableContent.clear();
                } else {
                    availableContent.clear();
                    availableContent.addAll(fetched);
                    statusText.setText("Loaded " + fetched.size() + " items from " + urlStr);
                }
                applyContentFilter();
            });
        });
    }

    private List<ContentItem> filteredContent() {
        String protocol = currentProtocol();
        String codec = currentCodec();
        List<ContentItem> out = new ArrayList<>();
        for (ContentItem c : availableContent) {
            boolean protocolOk = protocol.equals("HLS") ? c.hasHls : c.hasDash;
            if (!protocolOk) continue;
            if (!codec.equals("Auto") && !inferCodec(c.name).equals(codec)) continue;
            out.add(c);
        }
        return out;
    }

    private void applyContentFilter() {
        List<ContentItem> filtered = filteredContent();
        if (filtered.isEmpty()) {
            selectedContent = "";
            contentButton.setText(R.string.select_content);
            currentUrl = "";
            if (player != null) {
                player.stop();
                player.clearMediaItems();
            }
            return;
        }
        boolean stillValid = false;
        for (ContentItem c : filtered) {
            if (c.name.equals(selectedContent)) {
                stillValid = true;
                break;
            }
        }
        if (!stillValid) {
            selectedContent = filtered.get(0).name;
        }
        contentButton.setText(selectedContent);
        buildUrlAndLoad();
    }

    private void showContentPicker() {
        final List<ContentItem> filtered = filteredContent();
        if (filtered.isEmpty()) {
            Toast.makeText(this, "No content matches filters", Toast.LENGTH_SHORT).show();
            return;
        }
        String[] names = new String[filtered.size()];
        int preselect = 0;
        for (int i = 0; i < filtered.size(); i++) {
            names[i] = filtered.get(i).name;
            if (names[i].equals(selectedContent)) preselect = i;
        }
        new AlertDialog.Builder(this)
            .setTitle("Select Content (" + filtered.size() + ")")
            .setSingleChoiceItems(names, preselect, (dialog, which) -> {
                selectedContent = filtered.get(which).name;
                contentButton.setText(selectedContent);
                dialog.dismiss();
                buildUrlAndLoad();
            })
            .setNegativeButton("Cancel", null)
            .show();
    }

    private void buildUrlAndLoad() {
        if (selectedContent.isEmpty()) return;
        ServerEnvironment server = currentServer();
        String protocol = currentProtocol();
        String segment = currentSegment();
        String segmentSuffix = segment.equals("LL") ? "" : ("_" + segment.toLowerCase(Locale.US));
        String manifestFile = protocol.equals("HLS")
            ? ("master" + segmentSuffix + ".m3u8")
            : ("manifest" + segmentSuffix + ".mpd");
        currentUrl = String.format(Locale.US, "http://%s:%s/go-live/%s/%s?player_id=%s",
            server.host, server.port, selectedContent, manifestFile, playerId);
        loadStream(currentUrl);
    }

    private void loadStream(String url) {
        if (player != null && !url.isEmpty()) {
            MediaItem mediaItem = MediaItem.fromUri(url);
            player.setMediaItem(mediaItem);
            player.prepare();
            player.setPlayWhenReady(true);
            statusText.setText(url);
            if (metrics != null) metrics.onPlaybackStarted();
        }
    }

    private void retryFetch() {
        if (!currentUrl.isEmpty()) loadStream(currentUrl);
    }

    private void restartPlayback() {
        if (player != null) {
            if (metrics != null) metrics.onRestart("manual");
            player.stop();
            player.clearMediaItems();
            buildUrlAndLoad();
        }
    }

    private void toggleFullscreen() {
        isFullscreen = !isFullscreen;
        ConstraintLayout.LayoutParams params =
            (ConstraintLayout.LayoutParams) panelGuideline.getLayoutParams();
        params.guidePercent = isFullscreen ? 1.0f : 0.65f;
        panelGuideline.setLayoutParams(params);
        rightPanel.setVisibility(isFullscreen ? View.GONE : View.VISIBLE);
        fullscreenButton.setText(isFullscreen
            ? getString(R.string.exit_fullscreen)
            : getString(R.string.fullscreen));
    }

    @Override
    public void onBackPressed() {
        if (isFullscreen) {
            toggleFullscreen();
        } else {
            super.onBackPressed();
        }
    }

    @Override
    protected void onStart() {
        super.onStart();
        if (player != null) player.setPlayWhenReady(true);
        if (metrics != null) metrics.start();
    }

    @Override
    protected void onStop() {
        super.onStop();
        if (player != null) player.setPlayWhenReady(false);
        if (metrics != null) metrics.stop();
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        if (metrics != null) {
            metrics.release();
            metrics = null;
        }
        if (player != null) {
            player.release();
            player = null;
        }
    }
}
