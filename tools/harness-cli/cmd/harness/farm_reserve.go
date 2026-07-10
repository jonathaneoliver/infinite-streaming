package main

// farm_reserve.go is the synchronized-fleet slot RESERVATION primitive (issue
// #952). A comparison / isolation-fan job needs N devices simultaneously under
// one bandwidth timeline (the HOME barrier); if the streaming pool (#950) grabs
// one mid-bring-up the comparison desyncs. Reservation blocks N matching free
// devices on the farm up front — a blocked device drops out of availableDevices'
// Free set (verified live), so the pool never claims it — and the fleet then
// drives exactly those reserved devices. Released (unblocked) when the job ends.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// farmSetBlock blocks or unblocks one farm device (POST /device-farm/api/{block
// |unblock}). Block reserves it (removes it from the free roster); unblock
// releases it. host is sent alongside udid to match the farm's unblock contract.
func farmSetBlock(client *http.Client, base, udid, host string, block bool) error {
	action := "unblock"
	if block {
		action = "block"
	}
	body, _ := json.Marshal(map[string]string{"udid": udid, "host": host})
	req, err := http.NewRequest(http.MethodPost, base+"/device-farm/api/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("farm %s %s: HTTP %d", action, udid, resp.StatusCode)
	}
	return nil
}

// reserveDevices atomically reserves n free devices matching `match` (nil = any),
// so a synchronized fleet job holds its slots against the streaming pool (#952).
// All-or-nothing: if fewer than n match it reserves none; if a block fails
// mid-way it rolls back the ones it took. Returns the reserved UDIDs — pass them
// to the fleet (CHAR_FLEET_UDIDS) and to releaseDevices when done.
func reserveDevices(base string, n int, match func(DeviceCapability) bool) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	var cands []DeviceCapability
	for _, d := range FreeDevices(availableDevices(base)) {
		if match == nil || match(d) {
			cands = append(cands, d)
		}
	}
	if len(cands) < n {
		return nil, fmt.Errorf("reserve: need %d matching free device(s), only %d available", n, len(cands))
	}
	client := &http.Client{Timeout: 6 * time.Second}
	var reserved []string
	for _, d := range cands[:n] {
		if err := farmSetBlock(client, base, d.UDID, d.Host, true); err != nil {
			releaseDevices(base, reserved) // roll back — reservation is atomic
			return nil, fmt.Errorf("reserve %s: %w", d.UDID, err)
		}
		reserved = append(reserved, d.UDID)
	}
	return reserved, nil
}

// releaseDevices unblocks previously-reserved devices (best-effort; a failed
// unblock is left for the farm's own idle reaper / `farm.sh unblock_stuck`).
func releaseDevices(base string, udids []string) {
	if len(udids) == 0 {
		return
	}
	client := &http.Client{Timeout: 6 * time.Second}
	byUDID := map[string]string{}
	for _, d := range availableDevices(base) {
		byUDID[d.UDID] = d.Host
	}
	for _, u := range udids {
		_ = farmSetBlock(client, base, u, byUDID[u], false)
	}
}
