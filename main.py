import requests
import time
import json

def validate_provider_credentials():
    with open('config.json', 'r') as file:
        config = json.load(file)
    changes_made = False
    providers = config.get("provider_preferences", {})

    for company_name, settings in providers.items():
        if settings.get("enabled") == True:
            for key, value in settings.items():
                if key != "enabled" and (value == "" or "your_" in str(value).lower()):
                    print("\n" + "!"*50)
                    print(f"ACTION REQUIRED: {company_name.upper()} '{key}' is missing!")
                    new_value = input(f"Paste your {company_name} {key} here: ")

                    if new_value.strip() != "":
                        config["provider_preferences"][company_name][key] = new_value.strip()
                        changes_made = True
                        print(f"{key} saved")

    if changes_made:
        with open('config.json', 'w') as file:
            json.dump(config, file, indent=4)
            print("\nConfiguration updated. Initializing...\n")
    return config

config = validate_provider_credentials()

API_TOKEN = config["provider_preferences"]["tensordock"]["api_token"]
MAX_PRICE_PER_HOUR = config["budget"]["max_price_per_hour"]
TARGET_GPUS = config["hardware_requirements"]["target_gpus"]
PREFERRED_REGIONS = config["geography"]["preferred_regions"]

GPU_LADDER = {
    "rtx 5090": 100,
    "rtx 4090": 95,
    "rtx 4080": 85,
    "rtx 3090 ti": 82,
    "rtx 3090": 80,
    "rtx 4070": 70,
    "rtx 3080": 65,
    "rtx a6000": 60,
    "rtx a5000": 50,
    "rtx 4060": 40,
    "rtx 3060": 30
}

def get_gpu_score(gpu_name):
    gpu_name_lower = gpu_name.lower()

    for key, score in GPU_LADDER.items():
        if key in gpu_name_lower:
            return score
    return 0

def get_location_score(provider_location_string):
    loc_lower = provider_location_string.lower()

    for region in PREFERRED_REGIONS:
        if region.lower() in loc_lower:
            return 15
    return 0

def fetch_tensordock_candidates():
    print(f"[{time.strftime('%X')}] Fetching candidate servers from TensorDock...")
    url = "https://dashboard.tensordock.com/api/v2/locations"
    headers = {
        "Authorization": f"Bearer {API_TOKEN}", "Accept": "application/json"
    }
    candidates = []

    try:
        response = requests.get(url, headers=headers)
        response.raise_for_status()
        hostnodes = response.json().get("data", {}).get("locations", []) # Return the list of available hostnodes

        for location in hostnodes:
            city = location.get("city", "Unknown")
            state = location.get("stateprovince", "Unknown")
            country = location.get("country", "")

            if "united states" not in country.lower() and "us" not in country.lower():
                continue

            for gpu in location.get("gpus", []):
                gpu_name = gpu.get("displayName", "Unknown GPU")
                gpu_internal_id = gpu.get("v0Name", "")

                try: price = float(gpu.get("price_per_hr", 999.0))
                except: price = 999.0

                resources = gpu.get("resources", {})
                max_vcpus = resources.get("max_vcpus", 0)
                max_ram = resources.get("max_ram", 0)
                is_target = any(t.lower() in gpu_name.lower() for t in TARGET_GPUS)

                if is_target and price <= MAX_PRICE_PER_HOUR: ## and max_vcpus >= 8 and max_ram >= 16:
                    score = get_gpu_score(gpu_name)
                    loc_score = get_location_score(f"{city} {state}")
                    price_bonus = (MAX_PRICE_PER_HOUR - price) * 30
                    total_score = loc_score + score + price_bonus

                    candidates.append({
                        "provider": "TensorDock",
                        "deploy_id": location.get("id"),
                        "city": city,
                        "state": state,
                        "gpu_name": gpu_name,
                        "deploy_gpu_id": gpu_internal_id,
                        "price": price,
                        "max_vcpus": max_vcpus,
                        "max_ram": max_ram,
                        "score": round(total_score, 2)
                    })
        return candidates

    except Exception as e:
        print(f"TensorDock server connection error: {e}")
        return []

def fetch_gc_candidates():
    # Placeholder for Google Cloud fetching logic
    return []

def deploy_machine(location_id, gpu_v0_name):
    print("\n" + "!"*50)
    print(f"Deploying machine {location_id}...")
    print("!"*50 + "\n")

    url = "https://dashboard.tensordock.com/api/v2/instances"
    headers = {
        "Authorization": f"Bearer {API_TOKEN}",
        "Content-Type": "application/json",
        "Accept": "application/json"
    }

    deploy_payload = {
        "data": {
            "type": "virtualmachine",
            "attributes": {
                "name": "my-machine",
                "image": "windows10",
                "location_id": location_id,
                "password": "Awesome11107@13",
                "resources": {
                    "vcpu_count": 8,
                    "ram_gb": 16,
                    "storage_gb": 300,
                    "gpus": {
                        gpu_v0_name: { # We put the internal GPU barcode here
                            "count": 1
                            }
                        }
                    },
                "useDedicatedIp": False,
                "cloud_init": {
                    "runcmd": [
                        "powershell -ExecutionPolicy Bypass -Command \"Invoke-WebRequest -Uri 'https://raw.githubusercontent.com/NoviceAtPython/CloudDeploy-mover/refs/heads/main/CloudDeploy.ps1?token=GHSAT0AAAAAADYYPT7OYD7NY7D43E4TB2AQ2OU2OUQ' -OutFile 'C:\\deploy.ps1'; & 'C:\\deploy.ps1'\""
                    ]
                }
            }
        }
    }

    try:
        response = requests.post(url, headers=headers, json=deploy_payload)
        response.raise_for_status()
        print(f"Machine deployed successfully and is executing the shell script")
        print("Check your Tailscale dashboard in about 3 minutes")
        exit()
    except requests.exceptions.RequestException as e:
        print(f"Deployment failed (Like due to OS being unsupported at location): {e}")

    if hasattr(response, 'text'):
        print(f"API Error. Reason: {response.text}")

def execute_hunt():
    global_pool = []
    global_pool.extend(fetch_tensordock_candidates())

    if not global_pool:
        print("No candidates found. Retrying in 15 seconds...")
        time.sleep(15)
        return False

    print(f"Found {len(global_pool)} candidates. Finding best candidate...")

    global_pool.sort(key=lambda x: (-x['score'], x['price']))
    print("\n--- THE CANDIDATE POOL (TOP 8)---")

    for i, m in enumerate(global_pool[:8]):
        print(f"{i+1}. [Score: {m['score']}] {m['gpu_name']} in {m['city']} (${m['price']}/hr) via {m['provider']}")

    winner = global_pool[0]
    print("\n" + "="*50)
    print("Found most suitable server...")
    print(f"Executing deployment on {winner['provider']} for a {winner['gpu_name']} in {winner['city']} for (${winner['price']}/hr)")
    print("="*50 + "\n")

    if winner['provider'] == "TensorDock":
        ## deploy_machine(winner['deploy_id'], winner['deploy_gpu_id'])
        print("DRY RUN... Deployment skipped for testing purposes.")
        exit()


if __name__ == "__main__":
    print("Starting GPU hunt...")
    while True:
        execute_hunt()
        time.sleep(15)