from datetime import datetime


def generate_envoy_config(prediction: dict, cluster_name: str = "geo_backend") -> dict:
    zone1_pct = prediction["traffic_split_zone1"]
    zone2_pct = prediction["traffic_split_zone2"]

    zone1_weight = round(zone1_pct * 100)
    zone2_weight = 100 - zone1_weight

    return {
        "cluster_name": cluster_name,
        "policy": "weighted",
        "endpoints": [
            {"locality": "zone1", "weight": zone1_weight},
            {"locality": "zone2", "weight": zone2_weight},
        ],
        "model_metadata": {
            "version": prediction["model_version"],
            "confidence": prediction["confidence"],
            "generated_at": datetime.utcnow().isoformat() + "Z",
        },
    }
