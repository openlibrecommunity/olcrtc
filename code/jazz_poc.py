#!/usr/bin/env python3
"""PoC: SaluteJazz DataChannel over LiveKit."""

import asyncio
import io
import json
import logging
import time
import uuid
import aiohttp
from aiortc import RTCConfiguration, RTCIceCandidate, RTCIceServer, RTCPeerConnection, RTCSessionDescription
from aiortc.mediastreams import AudioStreamTrack
from aiortc.rtcconfiguration import RTCBundlePolicy

logging.getLogger("aiortc").setLevel(logging.WARNING)

API_BASE = "https://bk.salutejazz.ru"
JAZZ_HEADERS = {"X-Jazz-ClientId": str(uuid.uuid4()), "X-Jazz-AuthType": "ANONYMOUS", "X-Client-AuthType": "ANONYMOUS", "Content-Type": "application/json"}
TEST_MESSAGES = ["Hello Jazz DC!", "Hello world", "X" * 100, "Final test"]

def _pb_varint(v: int) -> bytes:
    b = bytearray()
    while v > 0x7F: b.append((v & 0x7F) | 0x80); v >>= 7
    b.append(v & 0x7F)
    return bytes(b)

def _pb_field(f: int, w: int, d: bytes) -> bytes:
    t = _pb_varint((f << 3) | w)
    return t + d if w == 0 else (t + _pb_varint(len(d)) + d if w == 2 else t + d)

def _read_varint(s: io.BytesIO) -> int | None:
    res, shift = 0, 0
    while b := s.read(1):
        res |= (b[0] & 0x7F) << shift
        if not (b[0] & 0x80): return res
        shift += 7
    return None

def encode_data_packet(payload: bytes, topic: str = "") -> bytes:
    uf = _pb_field(2, 2, payload) + (_pb_field(4, 2, topic.encode()) if topic else b"") + _pb_field(8, 2, str(uuid.uuid4()).encode())
    return _pb_field(1, 0, _pb_varint(0)) + _pb_field(2, 2, uf)

def decode_data_packet(raw: bytes) -> tuple[bytes, str] | None:
    s = io.BytesIO(raw)
    ud = None
    while (tg := _read_varint(s)) is not None:
        wt = tg & 0x07
        if wt == 0: _read_varint(s)
        elif wt == 2:
            l = _read_varint(s)
            if l is None: break
            d = s.read(l)
            if (tg >> 3) == 2: ud = d
        elif wt == 1: s.read(8)
        elif wt == 5: s.read(4)
        else: break
    if ud is None: return None
    p, t, ins = b"", "", io.BytesIO(ud)
    while (tg := _read_varint(ins)) is not None:
        wt = tg & 0x07
        if wt == 0: _read_varint(ins)
        elif wt == 2:
            l = _read_varint(ins)
            if l is None: break
            d = ins.read(l)
            fn = tg >> 3
            if fn == 2: p = d
            elif fn == 4: t = d.decode(errors="replace")
        elif wt == 1: ins.read(8)
        elif wt == 5: ins.read(4)
        else: break
    return p, t

async def _create_peer(name: str, room: dict, session: aiohttp.ClientSession, is_server: bool = False, stats: dict = None) -> dict:
    ws = await session.ws_connect(room["connectorUrl"])
    await ws.send_json({"roomId": room["roomId"], "event": "join", "requestId": str(uuid.uuid4()), "payload": {"password": room["password"], "participantName": name, "supportedFeatures": {"attachedRooms": True, "sessionGroups": True}, "isSilent": False}})
    
    peer = {"ws": ws, "pc_sub": None, "pc_pub": None, "dc": None, "ready": asyncio.Event(), "sub_ready": asyncio.Event()}
    group_id, p_ice_sub, p_ice_pub = None, [], []
    ice_servers = []

    async def ws_loop():
        nonlocal group_id
        async for msg in ws:
            if msg.type == aiohttp.WSMsgType.TEXT:
                data = json.loads(msg.data)
                ev = data.get("event", "")
                p = data.get("payload", {})
                m = p.get("method", "")

                if ev == "join-response": group_id = p.get("participantGroup", {}).get("groupId")
                elif ev == "media-out" and m == "rtc:config":
                    for s in p.get("configuration", {}).get("iceServers", []):
                        urls = [u for u in s.get("urls", []) if "transport=udp" in u]
                        if urls: ice_servers.append(RTCIceServer(urls=[urls[0]], username=s.get("username"), credential=s.get("credential")))
                
                elif ev == "media-out" and m == "rtc:offer" and not peer["pc_sub"]:
                    peer["pc_sub"] = RTCPeerConnection(configuration=RTCConfiguration(iceServers=ice_servers, bundlePolicy=RTCBundlePolicy.MAX_BUNDLE))
                    @peer["pc_sub"].on("connectionstatechange")
                    def _():
                        if peer["pc_sub"].connectionState == "connected": peer["sub_ready"].set()
                    
                    @peer["pc_sub"].on("datachannel")
                    def on_dc(ch):
                        if ch.label != "_reliable": return
                        @ch.on("message")
                        def on_msg(msg_data):
                            parsed = decode_data_packet(msg_data if isinstance(msg_data, bytes) else msg_data.encode())
                            if not parsed or parsed[1] != "poc": return
                            stats["recv"] += 1
                            if is_server and peer["dc"]:
                                try:
                                    peer["dc"].send(encode_data_packet(f"Echo: {parsed[0].decode()}".encode(), "poc"))
                                    stats["sent"] += 1
                                except: pass
                    
                    @peer["pc_sub"].on("icecandidate")
                    async def on_sub_ice(e):
                        if e and e.candidate and group_id:
                            await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:ice", "rtcIceCandidates": [{"candidate": e.candidate.candidate, "sdpMid": e.candidate.sdpMid, "sdpMLineIndex": e.candidate.sdpMLineIndex, "usernameFragment": "", "target": "SUBSCRIBER"}]}})
                    
                    await peer["pc_sub"].setRemoteDescription(RTCSessionDescription(sdp=p["description"]["sdp"], type="offer"))
                    ans = await peer["pc_sub"].createAnswer()
                    await peer["pc_sub"].setLocalDescription(ans)
                    await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:answer", "description": {"type": "answer", "sdp": peer["pc_sub"].localDescription.sdp}}})
                    for c in p_ice_sub:
                        pts = c.get("candidate","").split()
                        if len(pts) >= 8: await peer["pc_sub"].addIceCandidate(RTCIceCandidate(int(pts[1]), pts[0].split(":")[1], pts[4], int(pts[5]), int(pts[3]), pts[2], pts[7], str(c.get("sdpMid", "0")), c.get("sdpMLineIndex", 0)))
                    p_ice_sub.clear()
                    await asyncio.sleep(0.3)
                    
                    # pub
                    peer["pc_pub"] = RTCPeerConnection(configuration=RTCConfiguration(iceServers=ice_servers, bundlePolicy=RTCBundlePolicy.MAX_BUNDLE))
                    peer["pc_pub"].addTrack(AudioStreamTrack())
                    peer["dc"] = peer["pc_pub"].createDataChannel("_reliable", ordered=True)
                    
                    @peer["dc"].on("open")
                    def on_open(): peer["ready"].set()
                    
                    @peer["dc"].on("message")
                    def on_pub_msg(msg_data):
                        parsed = decode_data_packet(msg_data if isinstance(msg_data, bytes) else msg_data.encode())
                        if parsed and parsed[1] == "poc": stats["recv"] += 1
                            
                    @peer["pc_pub"].on("icecandidate")
                    async def on_pub_ice(e):
                        if e and e.candidate and group_id:
                            await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:ice", "rtcIceCandidates": [{"candidate": e.candidate.candidate, "sdpMid": e.candidate.sdpMid, "sdpMLineIndex": e.candidate.sdpMLineIndex, "usernameFragment": "", "target": "PUBLISHER"}]}})
                            
                    await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:track:add", "cid": str(uuid.uuid4()), "track": {"type": "AUDIO", "source": "MICROPHONE", "muted": True}}})
                    pub_offer = await peer["pc_pub"].createOffer()
                    await peer["pc_pub"].setLocalDescription(pub_offer)
                    await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:offer", "description": {"type": "offer", "sdp": peer["pc_pub"].localDescription.sdp}}})

                elif ev == "media-out" and m == "rtc:answer" and peer["pc_pub"]:
                    await peer["pc_pub"].setRemoteDescription(RTCSessionDescription(sdp=p["description"]["sdp"], type="answer"))
                    for c in p_ice_pub:
                        pts = c.get("candidate","").split()
                        if len(pts) >= 8: await peer["pc_pub"].addIceCandidate(RTCIceCandidate(int(pts[1]), pts[0].split(":")[1], pts[4], int(pts[5]), int(pts[3]), pts[2], pts[7], str(c.get("sdpMid", "0")), c.get("sdpMLineIndex", 0)))
                    p_ice_pub.clear()
                    
                elif ev == "media-out" and m == "rtc:ice":
                    for c in p.get("rtcIceCandidates", []):
                        pts = c.get("candidate","").split()
                        if len(pts) < 8: continue
                        ice = RTCIceCandidate(int(pts[1]), pts[0].split(":")[1], pts[4], int(pts[5]), int(pts[3]), pts[2], pts[7], str(c.get("sdpMid", "0")), c.get("sdpMLineIndex", 0))
                        tgt = c.get("target")
                        if tgt == "SUBSCRIBER": (await peer["pc_sub"].addIceCandidate(ice)) if peer["pc_sub"] else p_ice_sub.append(c)
                        elif tgt == "PUBLISHER": (await peer["pc_pub"].addIceCandidate(ice)) if peer["pc_pub"] else p_ice_pub.append(c)

    async def _keep():
        while not ws.closed:
            await asyncio.sleep(5)
            if group_id: await ws.send_json({"roomId": room["roomId"], "event": "media-in", "groupId": group_id, "requestId": str(uuid.uuid4()), "payload": {"method": "rtc:ping", "ping_req": {"timestamp": int(time.time()*1000), "rtt": 0}}})

    peer["task"] = asyncio.create_task(ws_loop())
    peer["keep"] = asyncio.create_task(_keep())
    return peer

async def run_poc() -> dict:
    print("\n--- SaluteJazz PoC ---")
    results = {"server_ok": False, "client_ok": False, "sent": 0, "recv": 0, "errors": []}
    s_stats, c_stats = {"sent": 0, "recv": 0}, {"sent": 0, "recv": 0}
    
    async with aiohttp.ClientSession() as session:
        try:
            r = await session.post(f"{API_BASE}/room/create-meeting", headers=JAZZ_HEADERS, json={"title": "PoC", "guestEnabled": True, "lobbyEnabled": False})
            rj = await r.json()
            r2 = await session.post(f"{API_BASE}/room/{rj['roomId']}/preconnect", headers=JAZZ_HEADERS, json={"password": rj["password"], "jazzNextMigration": {"b2bBaseRoomSupport": True, "demoRoomBaseSupport": True, "demoRoomVersionSupport": 2, "mediaWithoutAutoSubscribeSupport": True}})
            room_inf = {"roomId": rj["roomId"], "password": rj["password"], "connectorUrl": (await r2.json())["connectorUrl"]}
        except Exception as e:
            results["errors"].append(f"Auth fail: {e}")
            return results

        print("[1/3] Connecting Server & Client...")
        try:
            server = await _create_peer("Server", room_inf, session, is_server=True, stats=s_stats)
            await asyncio.wait_for(server["ready"].wait(), 15.0)
            results["server_ok"] = True
            
            client = await _create_peer("Client", room_inf, session, is_server=False, stats=c_stats)
            await asyncio.wait_for(client["ready"].wait(), 15.0)
            results["client_ok"] = True
            print(" :P Peers connected")
        except Exception as e:
            results["errors"].append(str(e))
            return results

        print("\n[2/3] Exchanging messages...")
        await asyncio.sleep(1)
        for idx, msg in enumerate(TEST_MESSAGES, 1):
            try:
                client["dc"].send(encode_data_packet(msg.encode(), "poc"))
                c_stats["sent"] += 1
                print(f" -> Sent: {msg}")
                await asyncio.sleep(0.5)
            except Exception as e:
                results["errors"].append(f"Sending {idx} failed: {str(e)}")

        await asyncio.sleep(3)
        results["sent"], results["recv"] = c_stats["sent"], c_stats["recv"]
        
        print("\n[3/3] Cleaning up...")
        for p in (server, client):
            for t in ["task", "keep"]: p[t].cancel()
            await p["ws"].close()
            for pc in [p["pc_sub"], p["pc_pub"]]:
                if pc: await pc.close()

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
