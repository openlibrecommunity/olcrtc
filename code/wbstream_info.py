#!/usr/bin/env python3
import asyncio
import json
import requests
from livekit import rtc

API_BASE = "https://stream.wb.ru"
WS_URL = "wss://wbstream01-el.wb.ru:7880"

def _get_room_token(room_id: str, display_name: str) -> tuple[str, str]:
    headers = {"User-Agent": "Mozilla/5.0 (Linux x86_64)", "Content-Type": "application/json"}
    
    print("[1/3] API Initialization...")
    reg_req = requests.post(f"{API_BASE}/auth/api/v1/auth/user/guest-register", json={"displayName": display_name, "device": {"deviceName": "Linux", "deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP"}}, headers=headers)
    reg_req.raise_for_status()
    auth_data = reg_req.json()
    print(" :P Guest registered")
    print(json.dumps(auth_data, indent=2))
    
    headers["Authorization"] = f"Bearer {auth_data['accessToken']}"
    
    if not room_id:
        print("\n[2/3] Room Preparation...")
        room_req = requests.post(f"{API_BASE}/api-room/api/v2/room", json={"roomType": "ROOM_TYPE_ALL_ON_SCREEN", "roomPrivacy": "ROOM_PRIVACY_FREE"}, headers=headers)
        room_req.raise_for_status()
        room_data = room_req.json()
        print(" :P Room created")
        print(json.dumps(room_data, indent=2))
        room_id = room_data["roomId"]
        
    print(f"\n[3/3] Fetching LiveKit token...")
    requests.post(f"{API_BASE}/api-room/api/v1/room/{room_id}/join", json={}, headers=headers).raise_for_status()
    tok_req = requests.get(f"{API_BASE}/api-room-manager/api/v1/room/{room_id}/token", params={"deviceType": "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP", "displayName": display_name}, headers=headers)
    tok_req.raise_for_status()
    token_data = tok_req.json()
    print(" :P Token received")
    print(json.dumps(token_data, indent=2))
    
    return room_id, token_data["roomToken"]

async def get_wb_info():
    print("\n--- WB Stream Info ---")
    try:
        room_id, token = _get_room_token("", "InfoBot")
    except Exception as e:
        print(f" X Auth failed: {e}"); return

    room = rtc.Room()

    @room.on("participant_connected")
    def on_participant_connected(p):
        print(f" -> Participant Connected: {p.identity} | Metadata: {p.metadata}")

    @room.on("track_subscribed")
    def on_track_subscribed(t, pub, p):
        print(f" -> Track Subscribed: {pub.name} ({t.kind}) from {p.identity}")

    print(f"\nConnecting to LiveKit: {WS_URL}")
    try:
        await room.connect(WS_URL, token)
        print(" :P Connected to room")
        print(f"\n--- Room State ---")
        
        sid = room.sid
        if asyncio.iscoroutine(sid): sid = await sid
        print(f"Room: {room.name} (SID: {sid})")
        print(f"Metadata: {room.metadata}")
        
        print("\nRemote Participants:")
        for sid, p in room.remote_participants.items():
            print(f" - {p.identity} | Metadata: {p.metadata}")
            for tsid, t in p.track_publications.items(): print(f"   * Track {t.name}: {t.kind}")

        print("\nWaiting 8s for events...")
        await asyncio.sleep(8)
    except Exception as e: print(f" X Connection error: {e}")
    finally: await room.disconnect()

    print("\n--- INFO COLLECTION COMPLETE ---")

if __name__ == "__main__":
    try: asyncio.run(get_wb_info())
    except KeyboardInterrupt: pass
