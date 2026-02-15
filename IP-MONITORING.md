# IP Monitoring Feature

## Overview
The IP monitoring feature captures and records the origination IP address of streaming session requests from external users. This enables audit tracking, troubleshooting, and security analysis.

## Features

### 1. Enhanced IP Extraction
- **X-Forwarded-For Support**: Extracts client IP from `X-Forwarded-For` header when available
- **Fallback to RemoteAddr**: Uses `RemoteAddr` when `X-Forwarded-For` is not present
- **External IP Detection**: Automatically identifies external IPs (non-private, non-loopback addresses)

### 2. Session Metadata
Each streaming session now includes:
- `origination_ip`: The IP address that initiated the session
- `origination_time`: Timestamp when the session was first created
- `is_external_ip`: Boolean flag indicating if the IP is external
- `x_forwarded_for`: Raw X-Forwarded-For header value

### 3. Logging
- External IP access events are logged with session details:
  ```
  [GO-PROXY][EXTERNAL-IP] session_id=123 player_id=hlsjs ip=203.0.113.45 user_agent="..."
  ```
- All requests include client IP in the log:
  ```
  [GO-PROXY][REQUEST] ... client_ip=203.0.113.45 ...
  ```

### 4. API Endpoints

#### GET /api/external-ips
Retrieves IP tracking data for all sessions.

**Query Parameters:**
- `filter=external` - Returns only sessions with external IPs

**Response:**
```json
{
  "entries": [
    {
      "session_id": "123",
      "player_id": "hlsjs",
      "origination_ip": "203.0.113.45",
      "origination_time": "2026-02-15T13:00:00.000",
      "last_request_time": "2026-02-15T13:05:00.000",
      "is_external": true,
      "user_agent": "Mozilla/5.0 ..."
    }
  ],
  "total": 1,
  "external_only": true
}
```

### 5. Dashboard UI

#### Session Details Panel
The session details panel in the testing session view displays:
- **Origination IP**: The IP that initiated the session, with an "EXTERNAL" badge for external IPs
- **Origination Time**: When the session was first created

#### External IP Monitoring Panel
A dedicated panel on the testing dashboard (`/dashboard/testing.html`) shows:
- List of all sessions with their origination IPs
- Filter to show only external IPs
- Session ID, Player ID, timestamps, and User Agent
- Visual badge for external IPs
- Refresh button to update the data

## Implementation Details

### Code Changes

#### go-proxy/cmd/server/main.go
- Added `extractClientIP()` function to parse IP from X-Forwarded-For or RemoteAddr
- Added `isExternalIP()` function to detect external IPs
- Modified `handleProxy()` to:
  - Extract and store origination IP on first request
  - Log external IP access events
  - Include client IP in request logs
- Added `handleGetExternalIPs()` API handler

#### content/dashboard/testing-session-ui.js
- Added display of origination IP and time in session details grid
- Added visual badge for external IPs

#### content/dashboard/testing.html
- Added External IP Monitoring panel
- Added `fetchExternalIPs()` and `renderExternalIPs()` functions
- Added event listeners for refresh and filter controls

#### content/shared-styles.css
- Added `.external-badge` CSS class for visual identification

## Security and Privacy Considerations

1. **IP Classification**: Only IPs that are not private, loopback, link-local, or unspecified are marked as external
2. **Data Storage**: IP data is stored in memory as part of session data (ephemeral)
3. **Access Control**: The `/api/external-ips` endpoint is available to authenticated dashboard users
4. **Logging**: External IP access is logged for audit purposes
5. **X-Forwarded-For Handling**: 
   - The implementation uses X-Forwarded-For header for IP extraction
   - **Important**: This header can be spoofed by clients
   - The application assumes deployment behind a trusted reverse proxy (nginx)
   - For production: Ensure the reverse proxy strips client-provided X-Forwarded-For headers
   - The proxy should only set the X-Forwarded-For header with the actual client IP
6. **Invalid IP Logging**: Invalid IP addresses are logged with warnings for debugging purposes

## Usage Examples

### Viewing Session Origination IP
1. Navigate to a testing session page
2. Expand the "Session Details" section
3. View the "Origination IP" field
4. External IPs will show an "EXTERNAL" badge

### Monitoring All External IPs
1. Navigate to `/dashboard/testing.html`
2. Scroll to the "External IP Monitoring" panel
3. Check/uncheck "External IPs Only" to filter
4. Click "Refresh" to update the data

### Querying via API
```bash
# Get all IPs
curl http://localhost:30081/api/external-ips

# Get only external IPs
curl http://localhost:30081/api/external-ips?filter=external
```

## Testing

To test the feature:
1. Start a streaming session from an external IP or use X-Forwarded-For header
2. Check the logs for `[GO-PROXY][EXTERNAL-IP]` entries
3. View the session details in the dashboard
4. Check the External IP Monitoring panel

## Future Enhancements

Potential future improvements:
- Persistent storage of IP data in database
- Statistical analysis and reporting
- GeoIP lookup integration
- Rate limiting based on IP
- Alert system for suspicious IP patterns
- Export functionality for IP data
