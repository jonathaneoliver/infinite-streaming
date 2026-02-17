"""Tests for HLS playlist parsing"""

import pytest
from tests.abrchar.playlist import parse_hls_master, Variant, Ladder


SAMPLE_MASTER_PLAYLIST = """#EXTM3U
#EXT-X-VERSION:6
#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",FRAME-RATE=30.000
360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000,AVERAGE-BANDWIDTH=2000000,RESOLUTION=960x540,CODECS="avc1.4d401f,mp4a.40.2",FRAME-RATE=30.000
540p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",FRAME-RATE=30.000
720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=10240000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",FRAME-RATE=60.000
1080p.m3u8
"""

MINIMAL_MASTER_PLAYLIST = """#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1000000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000
high.m3u8
"""


class TestPlaylistParsing:
    """Tests for HLS playlist parsing"""
    
    def test_parse_sample_playlist(self, tmp_path):
        """Test parsing a sample master playlist"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        assert len(ladder.variants) == 4
        # Variants should be sorted by bandwidth
        for i in range(1, len(ladder.variants)):
            assert ladder.variants[i].bandwidth >= ladder.variants[i-1].bandwidth
    
    def test_parse_bandwidth(self, tmp_path):
        """Test BANDWIDTH attribute parsing"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        # First variant (after sorting) should be 360p with 1.28 Mbps
        assert ladder.variants[0].bandwidth == 1280000
        assert abs(ladder.variants[0].get_bandwidth_mbps() - 1.28) < 0.01
    
    def test_parse_average_bandwidth(self, tmp_path):
        """Test AVERAGE-BANDWIDTH attribute parsing"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        # 360p variant has AVERAGE-BANDWIDTH
        variant = ladder.variants[0]
        assert variant.average_bandwidth == 1000000
        assert abs(variant.get_average_bandwidth_mbps() - 1.0) < 0.01
        
        # 1080p variant has no AVERAGE-BANDWIDTH
        variant = ladder.variants[3]
        assert variant.average_bandwidth is None
        # get_average_bandwidth_mbps should fall back to BANDWIDTH
        assert abs(variant.get_average_bandwidth_mbps() - 10.24) < 0.01
    
    def test_parse_resolution(self, tmp_path):
        """Test RESOLUTION attribute parsing"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        assert ladder.variants[0].resolution == "640x360"
        assert ladder.variants[1].resolution == "960x540"
    
    def test_parse_codecs(self, tmp_path):
        """Test CODECS attribute parsing"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        assert ladder.variants[0].codecs == "avc1.4d401e,mp4a.40.2"
    
    def test_parse_frame_rate(self, tmp_path):
        """Test FRAME-RATE attribute parsing"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        # 360p has 30fps
        assert abs(ladder.variants[0].frame_rate - 30.0) < 0.1
        
        # 1080p has 60fps
        assert abs(ladder.variants[3].frame_rate - 60.0) < 0.1
    
    def test_minimal_playlist(self, tmp_path):
        """Test minimal playlist with only BANDWIDTH"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(MINIMAL_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        assert len(ladder.variants) == 2
        assert ladder.variants[0].bandwidth == 1000000
        assert ladder.variants[1].bandwidth == 5000000
    
    def test_empty_playlist(self, tmp_path):
        """Test that empty playlist raises error"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text("#EXTM3U\n")
        
        with pytest.raises(ValueError, match="No variants found"):
            parse_hls_master(str(playlist_file))
    
    def test_find_variant_by_bandwidth(self, tmp_path):
        """Test finding variant by bandwidth"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        # Find variant closest to 1.5 Mbps (should be 360p with avg 1.0 Mbps)
        variant = ladder.find_variant_by_bandwidth(1.5)
        assert variant is not None
        assert variant.get_effective_bandwidth() == 1000000
        
        # Find variant closest to 10 Mbps (should be 1080p)
        variant = ladder.find_variant_by_bandwidth(10.0)
        assert variant is not None
        assert variant.bandwidth == 10240000
    
    def test_get_effective_bandwidth(self, tmp_path):
        """Test get_effective_bandwidth prefers AVERAGE-BANDWIDTH"""
        playlist_file = tmp_path / "master.m3u8"
        playlist_file.write_text(SAMPLE_MASTER_PLAYLIST)
        
        ladder = parse_hls_master(str(playlist_file))
        
        # With AVERAGE-BANDWIDTH
        variant = ladder.variants[0]
        assert variant.get_effective_bandwidth() == 1000000
        
        # Without AVERAGE-BANDWIDTH
        variant = ladder.variants[3]
        assert variant.get_effective_bandwidth() == variant.bandwidth
