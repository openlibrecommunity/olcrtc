#!/usr/bin/env python3

import asyncio
import json
import uuid
import time
import websockets
import requests
from urllib.parse import quote
from aiortc import RTCPeerConnection, RTCSessionDescription, RTCIceCandidate, RTCConfiguration, RTCIceServer

CONFERENCE_ID = "75047680642749"
CONFERENCE_URL = f"https://telemost.yandex.ru/j/{CONFERENCE_ID}"
API_BASE = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"

def generate_uuid():
    return str(uuid.uuid4())

def get_connection_info(display_name):
    url = f"{API_BASE}/conferences/{quote(CONFERENCE_URL, safe='')}/connection"
    params = {
        "next_gen_media_platform_allowed": "true",
        "display_name": display_name,
        "waiting_room_supported": "true"
    }
    
    headers = {
        "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0",
        "Accept": "*/*",
        "content-type": "application/json",
        "Client-Instance-Id": generate_uuid(),
        "X-Telemost-Client-Version": "187.1.0",
        "idempotency-key": generate_uuid(),
        "Origin": "https://telemost.yandex.ru",
        "Referer": "https://telemost.yandex.ru/"
    }
    
    response = requests.get(url, params=params, headers=headers)
    response.raise_for_status()
    return response.json()

async def create_peer(name, is_server=False):
    conn_info = get_connection_info(name)
    room_id = conn_info["room_id"]
    peer_id = conn_info["peer_id"]
    credentials = conn_info["credentials"]
    ws_url = conn_info["client_configuration"]["media_server_url"]
    
    pc_sub = RTCPeerConnection(RTCConfiguration(
        iceServers=[RTCIceServer(urls=["stun:stun.rtc.yandex.net:3478"])]
    ))
    
    pc_pub = RTCPeerConnection(RTCConfiguration(
        iceServers=[RTCIceServer(urls=["stun:stun.rtc.yandex.net:3478"])]
    ))
    
    dc_pub = pc_pub.createDataChannel("olcrtc", ordered=True)
    dc_ready = asyncio.Event()
    dc_active = None
    dc_pub_alive = False
    
    stats = {
        "sent": 0,
        "received": 0,
        "bytes_sent": 0,
        "bytes_received": 0,
        "messages": []
    }
    
    @dc_pub.on("open")
    def on_pub_open():
        nonlocal dc_pub_alive
        dc_pub_alive = True
        print(f"      [DC-PUB] {name} DataChannel OPENED: label={dc_pub.label}, state={dc_pub.readyState}")
    
    @dc_pub.on("close")
    def on_pub_close():
        nonlocal dc_pub_alive
        dc_pub_alive = False
        print(f"      [DC-PUB] {name} DataChannel CLOSED")
    
    @dc_pub.on("error")
    def on_pub_error(error):
        print(f"      [DC-PUB] {name} DataChannel ERROR: {error}")
    
    @dc_pub.on("message")
    def on_pub_msg(msg):
        msg_preview = msg[:50] if len(msg) > 50 else msg
        print(f"      [DC-PUB] {name} received message: {len(msg)} bytes, preview: {msg_preview}")
        stats["received"] += 1
        stats["bytes_received"] += len(msg)
        stats["messages"].append(("received", msg, time.time()))
    
    @pc_sub.on("datachannel")
    def on_sub_dc(channel):
        nonlocal dc_active
        print(f"      [DC-SUB] {name} received DataChannel: label={channel.label}, state={channel.readyState}")
        dc_active = channel
        dc_ready.set()
        
        @channel.on("open")
        def on_sub_open():
            print(f"      [DC-SUB] {name} DataChannel OPENED: label={channel.label}")
        
        @channel.on("close")
        def on_sub_close():
            print(f"      [DC-SUB] {name} DataChannel CLOSED: label={channel.label}")
        
        @channel.on("message")
        def on_message(message):
            msg_preview = message[:50] if len(message) > 50 else message
            print(f"      [DC-SUB] {name} received message: {len(message)} bytes, preview: {msg_preview}")
            stats["received"] += 1
            stats["bytes_received"] += len(message)
            stats["messages"].append(("received", message, time.time()))
            
            if is_server and dc_active:
                response = f"Echo: {message}"
                try:
                    dc_active.send(response)
                    stats["sent"] += 1
                    stats["bytes_sent"] += len(response)
                    stats["messages"].append(("sent", response, time.time()))
                    print(f"      [DC-ACTIVE] {name} sent echo: {len(response)} bytes")
                except Exception as e:
                    print(f"      [DC-ACTIVE] {name} send error: {e}")
    
    ws = await websockets.connect(ws_url)
    
    hello_msg = {
        "uid": generate_uuid(),
        "hello": {
            "participantMeta": {"name": name, "role": "SPEAKER", "sendAudio": False, "sendVideo": False},
            "participantAttributes": {"name": name, "role": "SPEAKER"},
            "sendAudio": False,
            "sendVideo": False,
            "sendSharing": False,
            "participantId": peer_id,
            "roomId": room_id,
            "serviceName": "telemost",
            "credentials": credentials,
            "capabilitiesOffer": {
                "offerAnswerMode": ["SEPARATE"],
                "initialSubscriberOffer": ["ON_HELLO"],
                "slotsMode": ["FROM_CONTROLLER"],
                "simulcastMode": ["DISABLED"],
                "selfVadStatus": ["FROM_SERVER"],
                "dataChannelSharing": ["TO_RTP"]
            },
            "sdkInfo": {"implementation": "python", "version": "1.0.0", "userAgent": f"OlcRTC-{name}"},
            "sdkInitializationId": generate_uuid(),
            "disablePublisher": False,
            "disableSubscriber": False
        }
    }
    
    await ws.send(json.dumps(hello_msg))
    
    publisher_sdp_sent = False
    
    async def ws_handler():
        nonlocal publisher_sdp_sent
        while True:
            try:
                data = json.loads(await ws.recv())
                
                if "serverHello" in data:
                    await ws.send(json.dumps({"uid": data["uid"], "ack": {"status": {"code": "OK"}}}))
                
                if "subscriberSdpOffer" in data and not publisher_sdp_sent:
                    await pc_sub.setRemoteDescription(RTCSessionDescription(
                        sdp=data["subscriberSdpOffer"]["sdp"], type="offer"
                    ))
                    
                    answer = await pc_sub.createAnswer()
                    await pc_sub.setLocalDescription(answer)
                    
                    await ws.send(json.dumps({
                        "uid": generate_uuid(),
                        "subscriberSdpAnswer": {
                            "pcSeq": data["subscriberSdpOffer"]["pcSeq"],
                            "sdp": pc_sub.localDescription.sdp
                        }
                    }))
                    
                    await ws.send(json.dumps({"uid": data["uid"], "ack": {"status": {"code": "OK"}}}))
                    await asyncio.sleep(0.3)
                    
                    pub_offer = await pc_pub.createOffer()
                    await pc_pub.setLocalDescription(pub_offer)
                    
                    await ws.send(json.dumps({
                        "uid": generate_uuid(),
                        "publisherSdpOffer": {
                            "pcSeq": 1,
                            "sdp": pc_pub.localDescription.sdp
                        }
                    }))
                    publisher_sdp_sent = True
                
                if "publisherSdpAnswer" in data:
                    await pc_pub.setRemoteDescription(RTCSessionDescription(
                        sdp=data["publisherSdpAnswer"]["sdp"], type="answer"
                    ))
                    await ws.send(json.dumps({"uid": data["uid"], "ack": {"status": {"code": "OK"}}}))
                
                if "webrtcIceCandidate" in data:
                    cand = data["webrtcIceCandidate"]
                    try:
                        parts = cand["candidate"].split()
                        if len(parts) >= 8:
                            ice = RTCIceCandidate(
                                component=int(parts[1]),
                                foundation=parts[0].replace("candidate:", ""),
                                ip=parts[4],
                                port=int(parts[5]),
                                priority=int(parts[3]),
                                protocol=parts[2],
                                type=parts[7],
                                sdpMid=cand["sdpMid"],
                                sdpMLineIndex=cand["sdpMlineIndex"]
                            )
                            
                            if cand.get("target") == "SUBSCRIBER":
                                await pc_sub.addIceCandidate(ice)
                            elif cand.get("target") == "PUBLISHER":
                                await pc_pub.addIceCandidate(ice)
                    except:
                        pass
            
            except:
                break
    
    @pc_pub.on("connectionstatechange")
    async def on_pub_state():
        print(f"      [PC-PUB] {name} connection state: {pc_pub.connectionState}")
    
    @pc_sub.on("connectionstatechange")
    async def on_sub_state():
        print(f"      [PC-SUB] {name} connection state: {pc_sub.connectionState}")
    
    @pc_sub.on("iceconnectionstatechange")
    async def on_sub_ice():
        print(f"      [PC-SUB] {name} ICE state: {pc_sub.iceConnectionState}")
    
    @pc_pub.on("iceconnectionstatechange")
    async def on_pub_ice():
        print(f"      [PC-PUB] {name} ICE state: {pc_pub.iceConnectionState}")
    
    @pc_sub.on("icecandidate")
    async def on_sub_ice(event):
        if event.candidate:
            await ws.send(json.dumps({
                "uid": generate_uuid(),
                "webrtcIceCandidate": {
                    "candidate": event.candidate.candidate,
                    "sdpMid": event.candidate.sdpMid,
                    "sdpMlineIndex": event.candidate.sdpMLineIndex,
                    "target": "SUBSCRIBER",
                    "pcSeq": 1
                }
            }))
    
    @pc_pub.on("icecandidate")
    async def on_pub_ice(event):
        if event.candidate:
            await ws.send(json.dumps({
                "uid": generate_uuid(),
                "webrtcIceCandidate": {
                    "candidate": event.candidate.candidate,
                    "sdpMid": event.candidate.sdpMid,
                    "sdpMlineIndex": event.candidate.sdpMLineIndex,
                    "target": "PUBLISHER",
                    "pcSeq": 1
                }
            }))
    
    ws_task = asyncio.create_task(ws_handler())
    
    return {
        "name": name,
        "dc_active": lambda: dc_active,
        "dc_pub": dc_pub,
        "dc_pub_alive": lambda: dc_pub_alive,
        "dc_ready": dc_ready,
        "stats": stats,
        "ws": ws,
        "ws_task": ws_task,
        "pc_sub": pc_sub,
        "pc_pub": pc_pub
    }

async def run_full_test():
    print(r"""
                  OlcRTC - Full Test Suite                       
          DataChannel over Yandex Telemost SFU                    
                    by zowue for olc
""")
    
    results = {
        "server_connected": False,
        "client_connected": False,
        "messages_sent": 0,
        "messages_received": 0,
        "bytes_sent": 0,
        "bytes_received": 0,
        "latency_ms": [],
        "errors": []
    }
    
    print("[1/4] Creating server peer...")
    try:
        server = await create_peer("Server", is_server=True)
        await asyncio.wait_for(server["dc_ready"].wait(), timeout=10.0)
        results["server_connected"] = True
        print("      :P Server connected")
    except Exception as e:
        results["errors"].append(f"Server failed: {e}")
        print(f"      X Error: {e}")
        return results
    
    print("\n[2/4] Creating client peer...")
    try:
        client = await create_peer("Client", is_server=False)
        await asyncio.wait_for(client["dc_ready"].wait(), timeout=10.0)
        results["client_connected"] = True
        print("      :P Client connected")
    except Exception as e:
        results["errors"].append(f"Client failed: {e}")
        print(f"      X Error: {e}")
        return results
    
    print("\n[3/4] Testing message exchange...")
    await asyncio.sleep(2)
    
    test_messages = [
        "Hello OlcRTC!",
        "я всего лиш хотел дружить зачем тролякатся",
        "X" * 100,
        "Final test"
    ]
    
    try:
        dc_client = client["dc_active"]()
        if not dc_client:
            raise Exception("Client DataChannel not available")
        
        print(f"      Using DataChannel: label={dc_client.label}, state={dc_client.readyState}")
        
        for i, msg in enumerate(test_messages, 1):
            send_time = time.time()
            
            try:
                dc_client.send(msg)
                print(f"      -> Sent via dc_active message {i}/{len(test_messages)} ({len(msg)}b)")
            except Exception as e:
                print(f"      X Failed to send via dc_active: {e}")
                
                if client["dc_pub_alive"]():
                    try:
                        client["dc_pub"].send(msg)
                        print(f"      -> Sent via dc_pub message {i}/{len(test_messages)} ({len(msg)}b)")
                    except Exception as e2:
                        print(f"      X Failed to send via dc_pub: {e2}")
            
            client["stats"]["sent"] += 1
            client["stats"]["bytes_sent"] += len(msg)
            await asyncio.sleep(0.5)
        
        await asyncio.sleep(3)
        
        results["messages_sent"] = client["stats"]["sent"]
        results["messages_received"] = client["stats"]["received"]
        results["bytes_sent"] = client["stats"]["bytes_sent"]
        results["bytes_received"] = client["stats"]["bytes_received"]
        
        print(f"      :P Sent: {results['messages_sent']} messages")
        print(f"      :P Received: {results['messages_received']} responses")
        
    except Exception as e:
        results["errors"].append(f"Exchange failed: {e}")
        print(f"      X Error: {e}")
    
    print("\n[4/4] Cleaning up...")
    try:
        server["ws_task"].cancel()
        client["ws_task"].cancel()
        await server["ws"].close()
        await client["ws"].close()
        await server["pc_sub"].close()
        await server["pc_pub"].close()
        await client["pc_sub"].close()
        await client["pc_pub"].close()
        print("      :P Cleanup complete")
    except:
        pass
    
    return results

def print_results(results):
    print(r"""
                       TEST RESULTS                              
""")
    
    print("Connection Status:")
    print(f"  - Server: {':P Connected' if results['server_connected'] else 'X Failed'}")
    print(f"  - Client: {':P Connected' if results['client_connected'] else 'X Failed'}")
    
    print("\nMessage Exchange:")
    print(f"  - Sent: {results['messages_sent']} messages ({results['bytes_sent']} bytes)")
    print(f"  - Received: {results['messages_received']} messages ({results['bytes_received']} bytes)")
    
    success_rate = (results['messages_received'] / results['messages_sent'] * 100) if results['messages_sent'] > 0 else 0
    print(f"  - Success Rate: {success_rate:.1f}%")
    
    if results['errors']:
        print("\nErrors:")
        for err in results['errors']:
            print(f"  - {err}")
    
    if results['messages_received'] > 0:
        print("\n :P TEST PASSED - OlcRTC PoC works!")
    else:
        print("\n  X TEST FAILED - Check errors above")

async def main():
    results = await run_full_test()
    print_results(results)

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n\nTest interrupted.")
