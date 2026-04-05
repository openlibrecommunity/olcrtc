#!/usr/bin/env python3

import asyncio
import json
import uuid
import websockets
import requests
from urllib.parse import quote
from aiortc import RTCPeerConnection, RTCSessionDescription, RTCIceCandidate, RTCConfiguration, RTCIceServer

CONFERENCE_ID = "33734896687006"
CONFERENCE_URL = f"https://telemost.yandex.ru/j/{CONFERENCE_ID}"
API_BASE = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"

CHUNK_SIZE = 7168
HEADER_SIZE = 1024

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

def chunk_data(data, transfer_id):
    total_size = len(data)
    chunk_count = (total_size + CHUNK_SIZE - 1) // CHUNK_SIZE
    packets = []
    
    for i in range(chunk_count):
        start = i * CHUNK_SIZE
        end = min(start + CHUNK_SIZE, total_size)
        chunk = data[start:end]
        
        header = {"tid": transfer_id, "idx": i, "total": chunk_count, "size": total_size}
        header_json = json.dumps(header).encode()
        header_padded = header_json.ljust(HEADER_SIZE, b'\x00')
        
        packets.append(header_padded + chunk)
    
    return packets

class ChunkedReceiver:
    def __init__(self):
        self.buffers = {}
        self.completed = {}
    
    def handle_chunk(self, packet):
        if len(packet) < HEADER_SIZE:
            return None
        
        header_bytes = packet[:HEADER_SIZE].rstrip(b'\x00')
        chunk_data = packet[HEADER_SIZE:]
        
        try:
            header = json.loads(header_bytes)
        except:
            return None
        
        tid = header["tid"]
        idx = header["idx"]
        total = header["total"]
        
        if tid not in self.buffers:
            self.buffers[tid] = {"chunks": {}, "total": total, "received": 0}
        
        buf = self.buffers[tid]
        
        if idx not in buf["chunks"]:
            buf["chunks"][idx] = chunk_data
            buf["received"] += 1
        
        if buf["received"] == buf["total"]:
            complete = b"".join(buf["chunks"][i] for i in range(buf["total"]))
            self.completed[tid] = complete
            del self.buffers[tid]
            return tid
        
        return None

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
    
    dc_pub = pc_pub.createDataChannel("dcsend", ordered=True)
    dc_pub_open = asyncio.Event()
    
    receiver = ChunkedReceiver()
    
    stats = {
        "sent": 0,
        "received": 0,
        "bytes_sent": 0,
        "bytes_received": 0,
        "messages": []
    }
    
    @dc_pub.on("open")
    def on_pub_open():
        dc_pub_open.set()
    
    @dc_pub.on("message")
    def on_pub_msg(msg):
        if isinstance(msg, str):
            stats["messages"].append(("text", msg))
        else:
            tid = receiver.handle_chunk(msg)
            if tid and tid in receiver.completed:
                data = receiver.completed[tid]
                stats["messages"].append(("data", data))
                del receiver.completed[tid]
    
    @pc_sub.on("datachannel")
    def on_sub_dc(channel):
        @channel.on("message")
        def on_message(message):
            if isinstance(message, str):
                stats["messages"].append(("text", message))
                
                if is_server and message.startswith("GET "):
                    url = message[4:].strip()
                    asyncio.create_task(handle_request(url, dc_pub, stats))
            else:
                tid = receiver.handle_chunk(message)
                if tid and tid in receiver.completed:
                    data = receiver.completed[tid]
                    stats["messages"].append(("data", data))
                    del receiver.completed[tid]
    
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
            "sdkInfo": {"implementation": "python", "version": "1.0.0", "userAgent": f"DCSend-{name}"},
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
        "dc_pub": dc_pub,
        "dc_pub_open": dc_pub_open,
        "stats": stats,
        "ws": ws,
        "ws_task": ws_task,
        "pc_sub": pc_sub,
        "pc_pub": pc_pub
    }

async def handle_request(url, dc, stats):
    try:
        if not url.startswith(('http://', 'https://')):
            url = 'https://' + url
        
        print(f"      -> Fetching {url}...")
        response = requests.get(url, timeout=10)
        response.raise_for_status()
        
        data = response.content
        print(f"      -> Got {len(data)} bytes, sending...")
        
        transfer_id = generate_uuid()
        packets = chunk_data(data, transfer_id)
        
        for packet in packets:
            while dc.bufferedAmount > CHUNK_SIZE * 2:
                await asyncio.sleep(0.001)
            dc.send(packet)
            stats["sent"] += 1
            stats["bytes_sent"] += len(packet)
        
        print(f"      :P Sent {len(packets)} chunks")
        
    except Exception as e:
        print(f"      X Error: {e}")
        try:
            dc.send(f"ERROR: {str(e)}")
        except:
            pass

async def run_dcsend():
    print(r"""
                  DCSend - DataChannel Transfer                  
           Request/Response over Yandex Telemost SFU            
                    by zowue for olc
""")
    
    print("[1/3] Creating server peer...")
    try:
        server = await create_peer("Server", is_server=True)
        await asyncio.wait_for(server["dc_pub_open"].wait(), timeout=10.0)
        print("      :P Server ready")
    except Exception as e:
        print(f"      X Error: {e}")
        return
    
    print("\n[2/3] Creating client peer...")
    try:
        client = await create_peer("Client", is_server=False)
        await asyncio.wait_for(client["dc_pub_open"].wait(), timeout=10.0)
        print("      :P Client ready")
    except Exception as e:
        print(f"      X Error: {e}")
        return
    
    print("\n[3/3] Starting transfer...")
    await asyncio.sleep(2)
    
    url = "zarazaex.xyz/curl.txt"
    print(f"      -> Client requesting: {url}")
    
    client["dc_pub"].send(f"GET {url}")
    
    print("      -> Waiting for response...")
    
    for _ in range(30):
        await asyncio.sleep(0.5)
        
        for msg_type, msg_data in client["stats"]["messages"]:
            if msg_type == "data":
                print(f"\n      :P Received {len(msg_data)} bytes")
                print("\n--- Response Content ---")
                try:
                    print(msg_data.decode('utf-8'))
                except:
                    print(f"[Binary data: {len(msg_data)} bytes]")
                print("--- End ---\n")
                
                client["stats"]["messages"].clear()
                
                print("\nCleaning up...")
                server["ws_task"].cancel()
                client["ws_task"].cancel()
                await server["ws"].close()
                await client["ws"].close()
                await server["pc_sub"].close()
                await server["pc_pub"].close()
                await client["pc_sub"].close()
                await client["pc_pub"].close()
                
                print(":P Transfer complete")
                return
            
            elif msg_type == "text" and msg_data.startswith("ERROR"):
                print(f"\n      X {msg_data}\n")
                client["stats"]["messages"].clear()
                return
    
    print("\n      X Timeout waiting for response")

async def main():
    await run_dcsend()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n\nTransfer interrupted.")
