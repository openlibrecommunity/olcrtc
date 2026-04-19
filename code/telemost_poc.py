#!/usr/bin/env python3
"""PoC: Yandex Telemost DataChannel via Websocket and AIORTC."""

import asyncio
import json
import uuid
import time
import requests
import websockets
from urllib.parse import quote
from aiortc import RTCPeerConnection, RTCSessionDescription, RTCIceCandidate, RTCConfiguration, RTCIceServer

CONFERENCE_ID = "75047680642749"
API_BASE = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"
ICE_SERVER = RTCIceServer(urls=["stun:stun.rtc.yandex.net:3478"])
TEST_MESSAGES = ["Hello Yandex Telemost!", "Hello world", "X" * 100, "Final test"]

def _gen_uuid() -> str: return str(uuid.uuid4())

def _get_conn_info(display_name: str) -> dict:
    url = f"{API_BASE}/conferences/{quote(f'https://telemost.yandex.ru/j/{CONFERENCE_ID}', safe='')}/connection"
    headers = {
        "User-Agent": "Mozilla/5.0 (Linux x86_64)",
        "Client-Instance-Id": _gen_uuid(),
        "X-Telemost-Client-Version": "187.1.0",
        "idempotency-key": _gen_uuid(),
    }
    params = {"next_gen_media_platform_allowed": "true", "display_name": display_name, "waiting_room_supported": "true"}
    resp = requests.get(url, params=params, headers=headers)
    resp.raise_for_status()
    return resp.json()

async def _create_peer(name: str, is_server: bool = False, stats: dict = None) -> dict:
    info = _get_conn_info(name)
    ws = await websockets.connect(info["client_configuration"]["media_server_url"])
    pc_sub = RTCPeerConnection(RTCConfiguration(iceServers=[ICE_SERVER]))
    pc_pub = RTCPeerConnection(RTCConfiguration(iceServers=[ICE_SERVER]))
    dc_pub = pc_pub.createDataChannel("olcrtc", ordered=True)
    dc_ready = asyncio.Event()

    @dc_pub.on("open")
    def on_open(): dc_ready.set()
    
    @dc_pub.on("message")
    def on_pub_msg(msg): stats["recv"] += 1
        
    @pc_sub.on("datachannel")
    def on_sub_dc(channel):
        @channel.on("message")
        def on_msg(m):
            stats["recv"] += 1
            if is_server and channel.label == "olcrtc":
                try:
                    dc_pub.send(f"Echo: {m}")
                    stats["sent"] += 1
                except: pass

    await ws.send(json.dumps({
        "uid": _gen_uuid(),
        "hello": {
            "participantMeta": {"name": name, "role": "SPEAKER", "sendAudio": False, "sendVideo": False},
            "participantAttributes": {"name": name, "role": "SPEAKER"},
            "sendAudio": False, "sendVideo": False, "sendSharing": False,
            "participantId": info["peer_id"], "roomId": info["room_id"],
            "serviceName": "telemost", "credentials": info["credentials"],
            "capabilitiesOffer": {"offerAnswerMode": ["SEPARATE"], "initialSubscriberOffer": ["ON_HELLO"], "slotsMode": ["FROM_CONTROLLER"], "simulcastMode": ["DISABLED"], "selfVadStatus": ["FROM_SERVER"], "dataChannelSharing": ["TO_RTP"]},
            "sdkInfo": {"implementation": "python", "version": "1.0.0", "userAgent": f"OlcRTC-{name}"},
            "sdkInitializationId": _gen_uuid(), "disablePublisher": False, "disableSubscriber": False
        }
    }))

    async def _ws_loop():
        pub_sdp_sent = False
        while True:
            try:
                data = json.loads(await ws.recv())
                uid = data.get("uid")
                
                if "serverHello" in data:
                    await ws.send(json.dumps({"uid": uid, "ack": {"status": {"code": "OK"}}}))
                    
                elif "subscriberSdpOffer" in data and not pub_sdp_sent:
                    sdp = data["subscriberSdpOffer"]
                    await pc_sub.setRemoteDescription(RTCSessionDescription(sdp=sdp["sdp"], type="offer"))
                    ans = await pc_sub.createAnswer()
                    await pc_sub.setLocalDescription(ans)
                    await ws.send(json.dumps({"uid": _gen_uuid(), "subscriberSdpAnswer": {"pcSeq": sdp["pcSeq"], "sdp": pc_sub.localDescription.sdp}}))
                    await ws.send(json.dumps({"uid": uid, "ack": {"status": {"code": "OK"}}}))
                    await asyncio.sleep(0.3)
                    pub_offer = await pc_pub.createOffer()
                    await pc_pub.setLocalDescription(pub_offer)
                    await ws.send(json.dumps({"uid": _gen_uuid(), "publisherSdpOffer": {"pcSeq": 1, "sdp": pc_pub.localDescription.sdp}}))
                    pub_sdp_sent = True
                    
                elif "publisherSdpAnswer" in data:
                    await pc_pub.setRemoteDescription(RTCSessionDescription(sdp=data["publisherSdpAnswer"]["sdp"], type="answer"))
                    await ws.send(json.dumps({"uid": uid, "ack": {"status": {"code": "OK"}}}))
                    
                elif "webrtcIceCandidate" in data:
                    cand = data["webrtcIceCandidate"]
                    parts = cand["candidate"].split()
                    if len(parts) >= 8:
                        ice = RTCIceCandidate(component=int(parts[1]), foundation=parts[0].replace("candidate:", ""), ip=parts[4], port=int(parts[5]), priority=int(parts[3]), protocol=parts[2], type=parts[7], sdpMid=cand["sdpMid"], sdpMLineIndex=cand["sdpMlineIndex"])
                        await (pc_sub if cand.get("target") == "SUBSCRIBER" else pc_pub).addIceCandidate(ice)
            except Exception: break

    async def _on_ice(event, target):
        if event.candidate:
            await ws.send(json.dumps({"uid": _gen_uuid(), "webrtcIceCandidate": {"candidate": event.candidate.candidate, "sdpMid": event.candidate.sdpMid, "sdpMlineIndex": event.candidate.sdpMLineIndex, "target": target, "pcSeq": 1}}))

    pc_sub.on("icecandidate", lambda e: asyncio.create_task(_on_ice(e, "SUBSCRIBER")))
    pc_pub.on("icecandidate", lambda e: asyncio.create_task(_on_ice(e, "PUBLISHER")))
    
    return {"dc": dc_pub, "ready": dc_ready, "task": asyncio.create_task(_ws_loop()), "ws": ws, "pc_sub": pc_sub, "pc_pub": pc_pub}

async def run_poc() -> dict:
    print("\n--- Yandex Telemost PoC ---")
    results = {"server_ok": False, "client_ok": False, "sent": 0, "recv": 0, "errors": []}
    s_stats, c_stats = {"sent": 0, "recv": 0}, {"sent": 0, "recv": 0}
    
    print("[1/3] Connecting Server & Client...")
    try:
        server = await _create_peer("Server", is_server=True, stats=s_stats)
        await asyncio.wait_for(server["ready"].wait(), 10.0)
        results["server_ok"] = True
        
        client = await _create_peer("Client", is_server=False, stats=c_stats)
        await asyncio.wait_for(client["ready"].wait(), 10.0)
        results["client_ok"] = True
        print(" :P Peers connected")
    except Exception as e:
        results["errors"].append(str(e))
        return results

    print("\n[2/3] Exchanging messages...")
    await asyncio.sleep(1)
    for idx, msg in enumerate(TEST_MESSAGES, 1):
        try:
            client["dc"].send(msg)
            c_stats["sent"] += 1
            print(f" -> Sent: {msg}")
            await asyncio.sleep(0.5)
        except Exception as e:
            results["errors"].append(f"Sending {idx} failed: {str(e)}")

    await asyncio.sleep(2)
    results["sent"], results["recv"] = c_stats["sent"], c_stats["recv"]
    
    print("\n[3/3] Cleaning up...")
    for p in (server, client):
        p["task"].cancel()
        await p["ws"].close()
        await p["pc_sub"].close()
        await p["pc_pub"].close()
        
    return results

def print_results(res: dict):
    print("\n--- TEST RESULTS ---")
    print(f"Server: {':P' if res['server_ok'] else 'X'} / Client: {':P' if res['client_ok'] else 'X'}")
    print(f"Messages: Sent {res['sent']} / Recv {res['recv']}")
    if res['errors']:
        for e in res['errors']: print(f" Error: {e}")
    print(f"\n{':P SUCCESS' if res['sent'] and res['sent'] == res['recv'] else 'X FAILED'}\n")

if __name__ == "__main__":
    try: res = asyncio.run(run_poc()); print_results(res)
    except KeyboardInterrupt: pass
