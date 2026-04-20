#!/usr/bin/env python3
import asyncio
import json
import uuid
import aiohttp
from urllib.parse import quote

API_BASE = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"
CONFERENCE_ID = "75047680642749"
ROOM_URL = f"https://telemost.yandex.ru/j/{CONFERENCE_ID}"

async def get_telemost_info():
    print("\n--- Yandex Telemost Info ---")
    async with aiohttp.ClientSession() as session:
        print(f"[1/3] Fetching connection info...")
        url = f"{API_BASE}/conferences/{quote(ROOM_URL, safe='')}/connection"
        params = {"next_gen_media_platform_allowed": "true", "display_name": "InfoBot", "waiting_room_supported": "true"}
        headers = {"User-Agent": "Mozilla/5.0 (Linux x86_64)", "Client-Instance-Id": str(uuid.uuid4()), "X-Telemost-Client-Version": "187.1.0", "idempotency-key": str(uuid.uuid4())}
        
        try:
            async with session.get(url, params=params, headers=headers) as resp:
                if resp.status != 200: print(f" X API Fail: {resp.status}"); return
                conn_info = await resp.json()
                print(" :P Connection data received")
                print(json.dumps(conn_info, indent=2))
        except Exception as e: print(f" X Error: {e}"); return

        print(f"\n[2/3] Connecting to signaling...")
        try:
            async with session.ws_connect(conn_info["client_configuration"]["media_server_url"]) as ws:
                await ws.send_json({"uid": str(uuid.uuid4()), "hello": {"participantMeta": {"name": "InfoBot", "role": "SPEAKER", "sendAudio": False, "sendVideo": False}, "participantAttributes": {"name": "InfoBot", "role": "SPEAKER"}, "sendAudio": False, "sendVideo": False, "sendSharing": False, "participantId": conn_info["peer_id"], "roomId": conn_info["room_id"], "serviceName": "telemost", "credentials": conn_info["credentials"], "capabilitiesOffer": {"offerAnswerMode": ["SEPARATE"], "initialSubscriberOffer": ["ON_HELLO"], "slotsMode": ["FROM_CONTROLLER"], "simulcastMode": ["DISABLED"], "selfVadStatus": ["FROM_SERVER"], "dataChannelSharing": ["TO_RTP"]}, "sdkInfo": {"implementation": "python", "version": "1.0.0", "userAgent": "OlcRTC-InfoBot"}, "sdkInitializationId": str(uuid.uuid4()), "disablePublisher": False, "disableSubscriber": False}})
                print(" :P Signaling established")

                print("\n[3/3] Collecting media details...")
                end = asyncio.get_event_loop().time() + 8
                while asyncio.get_event_loop().time() < end:
                    try:
                        m = await asyncio.wait_for(ws.receive(), 1)
                        if m.type == aiohttp.WSMsgType.TEXT:
                            d = json.loads(m.data); uid = d.get("uid")
                            print(f" -> Message: {list(d.keys())}")
                            if "serverHello" in d:
                                print("\n--- Server Hello / Telemetry ---")
                                print(json.dumps(d["serverHello"], indent=2))
                            elif "subscriberSdpOffer" in d:
                                print("\n--- SDP Offer (Codecs & Quality) ---")
                                print(d["subscriberSdpOffer"].get("sdp"))
                            elif "webrtcIceCandidate" in d:
                                print(f" -> ICE: {d['webrtcIceCandidate'].get('candidate')}")
                            if uid: await ws.send_json({"uid": uid, "ack": {"status": {"code": "OK"}}})
                    except: continue
        except Exception as e: print(f" X Signaling Fail: {e}")

    print("\n--- INFO COLLECTION COMPLETE ---")

if __name__ == "__main__":
    try: asyncio.run(get_telemost_info())
    except KeyboardInterrupt: pass
