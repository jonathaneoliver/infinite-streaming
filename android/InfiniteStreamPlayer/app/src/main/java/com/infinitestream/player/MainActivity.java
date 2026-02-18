package com.infinitestream.player;

import android.os.Bundle;
import android.view.View;
import android.widget.AdapterView;
import android.widget.ArrayAdapter;
import android.widget.Button;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

import androidx.annotation.OptIn;
import androidx.appcompat.app.AppCompatActivity;
import androidx.media3.common.MediaItem;
import androidx.media3.common.Player;
import androidx.media3.common.util.UnstableApi;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.ui.PlayerView;

import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

/**
 * InfiniteStream Player - One-page ExoPlayer app
 * 
 * A minimal, single-page interface demonstrating core ExoPlayer playback functionality.
 * Modeled after the Swift One app with clean UI and essential playback controls.
 */
@OptIn(markerClass = UnstableApi.class)
public class MainActivity extends AppCompatActivity {

    private ExoPlayer player;
    private PlayerView playerView;
    private Button retryButton;
    private Button restartButton;
    private Spinner serverSpinner;
    private Spinner protocolSpinner;
    private Spinner segmentSpinner;
    private Spinner codecSpinner;
    private Spinner contentSpinner;
    private TextView statusText;
    
    private String currentUrl = "";
    
    // Server environments
    private static class ServerEnvironment {
        String name;
        String host;
        String port;
        
        ServerEnvironment(String name, String host, String port) {
            this.name = name;
            this.host = host;
            this.port = port;
        }
        
        @Override
        public String toString() {
            return name + " (" + port + ")";
        }
    }
    
    private final ServerEnvironment[] servers = {
        new ServerEnvironment("Dev", "100.111.190.54", "40081"),
        new ServerEnvironment("Release", "infinitestreaming.jeoliver.com", "30081")
    };
    
    private final String[] protocols = {"HLS", "DASH"};
    private final String[] segments = {"LL", "2s", "6s"};
    private final String[] codecs = {"H.264", "H.265"};
    private final String[] contentItems = {
        "bbb",
        "counter-10m",
        "counter-1h",
        "sintel"
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        // Initialize views
        playerView = findViewById(R.id.player_view);
        retryButton = findViewById(R.id.retry_button);
        restartButton = findViewById(R.id.restart_button);
        serverSpinner = findViewById(R.id.server_spinner);
        protocolSpinner = findViewById(R.id.protocol_spinner);
        segmentSpinner = findViewById(R.id.segment_spinner);
        codecSpinner = findViewById(R.id.codec_spinner);
        contentSpinner = findViewById(R.id.content_spinner);
        statusText = findViewById(R.id.status_text);

        // Initialize ExoPlayer
        initializePlayer();

        // Setup spinners
        setupSpinners();

        // Setup buttons
        retryButton.setOnClickListener(v -> retryFetch());
        restartButton.setOnClickListener(v -> restartPlayback());
    }

    private void initializePlayer() {
        player = new ExoPlayer.Builder(this).build();
        playerView.setPlayer(player);
        
        // Add listener for playback events
        player.addListener(new Player.Listener() {
            @Override
            public void onPlaybackStateChanged(int state) {
                updateStatus(state);
            }
            
            @Override
            public void onPlayerError(androidx.media3.common.PlaybackException error) {
                statusText.setText("Error: " + error.getMessage());
                Toast.makeText(MainActivity.this, "Playback error: " + error.getMessage(), 
                    Toast.LENGTH_LONG).show();
            }
        });
    }

    private void setupSpinners() {
        // Server spinner
        ArrayAdapter<ServerEnvironment> serverAdapter = new ArrayAdapter<>(
            this, android.R.layout.simple_spinner_item, servers);
        serverAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        serverSpinner.setAdapter(serverAdapter);
        serverSpinner.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                buildUrlAndLoad();
            }
            @Override
            public void onNothingSelected(AdapterView<?> parent) {}
        });

        // Protocol spinner
        ArrayAdapter<String> protocolAdapter = new ArrayAdapter<>(
            this, android.R.layout.simple_spinner_item, protocols);
        protocolAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        protocolSpinner.setAdapter(protocolAdapter);
        protocolSpinner.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                buildUrlAndLoad();
            }
            @Override
            public void onNothingSelected(AdapterView<?> parent) {}
        });

        // Segment spinner
        ArrayAdapter<String> segmentAdapter = new ArrayAdapter<>(
            this, android.R.layout.simple_spinner_item, segments);
        segmentAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        segmentSpinner.setAdapter(segmentAdapter);
        segmentSpinner.setSelection(2); // Default to 6s
        segmentSpinner.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                buildUrlAndLoad();
            }
            @Override
            public void onNothingSelected(AdapterView<?> parent) {}
        });

        // Codec spinner
        ArrayAdapter<String> codecAdapter = new ArrayAdapter<>(
            this, android.R.layout.simple_spinner_item, codecs);
        codecAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        codecSpinner.setAdapter(codecAdapter);
        codecSpinner.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                buildUrlAndLoad();
            }
            @Override
            public void onNothingSelected(AdapterView<?> parent) {}
        });

        // Content spinner
        ArrayAdapter<String> contentAdapter = new ArrayAdapter<>(
            this, android.R.layout.simple_spinner_item, contentItems);
        contentAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        contentSpinner.setAdapter(contentAdapter);
        contentSpinner.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                buildUrlAndLoad();
            }
            @Override
            public void onNothingSelected(AdapterView<?> parent) {}
        });
    }

    private void buildUrlAndLoad() {
        ServerEnvironment server = (ServerEnvironment) serverSpinner.getSelectedItem();
        String protocol = (String) protocolSpinner.getSelectedItem();
        String segment = (String) segmentSpinner.getSelectedItem();
        String codec = (String) codecSpinner.getSelectedItem();
        String content = (String) contentSpinner.getSelectedItem();
        
        if (server == null || protocol == null || content == null) {
            return;
        }

        // Build URL based on selections
        String codecSuffix = codec.equals("H.265") ? "_h265" : "";
        String segmentSuffix = segment.equals("LL") ? "" : ("_" + segment.toLowerCase());
        String manifestFile = protocol.equals("HLS") ? 
            ("master" + segmentSuffix + ".m3u8") : 
            ("manifest" + segmentSuffix + ".mpd");
        
        currentUrl = String.format(Locale.US, "http://%s:%s/go-live/%s%s/%s",
            server.host, server.port, content, codecSuffix, manifestFile);
        
        loadStream(currentUrl);
    }

    private void loadStream(String url) {
        if (player != null && !url.isEmpty()) {
            MediaItem mediaItem = MediaItem.fromUri(url);
            player.setMediaItem(mediaItem);
            player.prepare();
            player.setPlayWhenReady(true);
            statusText.setText("Loading: " + url);
        }
    }

    private void retryFetch() {
        statusText.setText("Retrying fetch...");
        if (!currentUrl.isEmpty()) {
            loadStream(currentUrl);
        }
    }

    private void restartPlayback() {
        statusText.setText("Restarting playback...");
        if (player != null) {
            player.stop();
            player.clearMediaItems();
            buildUrlAndLoad();
        }
    }

    private void updateStatus(int playbackState) {
        String state = "";
        switch (playbackState) {
            case Player.STATE_IDLE:
                state = "Idle";
                break;
            case Player.STATE_BUFFERING:
                state = "Buffering...";
                break;
            case Player.STATE_READY:
                state = "Ready - Playing";
                break;
            case Player.STATE_ENDED:
                state = "Ended";
                break;
        }
        statusText.setText(state + " - " + currentUrl);
    }

    @Override
    protected void onStart() {
        super.onStart();
        if (player != null) {
            player.setPlayWhenReady(true);
        }
    }

    @Override
    protected void onStop() {
        super.onStop();
        if (player != null) {
            player.setPlayWhenReady(false);
        }
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        if (player != null) {
            player.release();
            player = null;
        }
    }
}
