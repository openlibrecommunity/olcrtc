#!/usr/bin/env python3
import asyncio
import json
import uuid
import aiohttp

API_BASE = "https://bk.salutejazz.ru"
JAZZ_HEADERS = {"X-Jazz-ClientId": str(uuid.uuid4()), "X-Jazz-AuthType": "ANONYMOUS", "X-Client-AuthType": "ANONYMOUS", "Content-Type": "application/json"}

async def get_jazz_info():
    print("\n--- SaluteJazz Info ---")
    timeout = aiohttp.ClientTimeout(total=15)
    async with aiohttp.ClientSession(timeout=timeout) as session:
        print("[1/4] API Initialization...")
        try:
            r = await session.post(f"{API_BASE}/room/create-meeting", headers=JAZZ_HEADERS, json={"title": "InfoBot", "guestEnabled": True, "lobbyEnabled": False, "room3dEnabled": False})
            rj = await r.json()
            print(" :P Room created")
            print(json.dumps(rj, indent=2))
            
            r2 = await session.post(f"{API_BASE}/room/{rj['roomId']}/preconnect", headers=JAZZ_HEADERS, json={"password": rj["password"], "jazzNextMigration": {"b2bBaseRoomSupport": True, "sdkRoomSupport": True, "mediaWithoutAutoSubscribeSupport": True}})
            r2j = await r2.json()
            print(" :P Preconnect info received")
            print(json.dumps(r2j, indent=2))
            conn_url = r2j['connectorUrl']
        except Exception as e:
            print(f" X Error: {e}"); return

        print(f"\n[2/4] Connecting to signaling...")
        async with session.ws_connect(conn_url) as ws:
            await ws.send_json({"roomId": rj["roomId"], "event": "join", "requestId": str(uuid.uuid4()), "payload": {"password": rj["password"], "participantName": "InfoBot", "supportedFeatures": {"attachedRooms": True}, "isSilent": False}})
            print(" :P Signaling established")

            print("\n[3/4] Collecting network & media details...")
            end = asyncio.get_event_loop().time() + 8
            while asyncio.get_event_loop().time() < end:
                try:
                    m = await asyncio.wait_for(ws.receive(), 1)
                    if m.type == aiohttp.WSMsgType.TEXT:
                        d = json.loads(m.data); ev = d.get("event", ""); p = d.get("payload", {}); meth = p.get("method", "")
                        print(f" -> Event: {ev}{' ('+meth+')' if meth else ''}")
                        if meth == "rtc:config":
                            print("\n--- ICE Servers ---")
                            print(json.dumps(p.get("configuration", {}).get("iceServers", []), indent=2))
                        elif meth == "rtc:offer":
                            print("\n--- SDP Offer (Codecs & Quality) ---")
                            print(p.get("description", {}).get("sdp", ""))
                        elif ev == "join-response":
                            print("\n--- Participant Group ---")
                            print(json.dumps(p.get("participantGroup", {}), indent=2))
                        else:
                            print(json.dumps(p, indent=2))
                except: continue

    print("\n--- INFO COLLECTION COMPLETE ---")

if __name__ == "__main__":
    try: asyncio.run(get_jazz_info())
    except KeyboardInterrupt: pass
