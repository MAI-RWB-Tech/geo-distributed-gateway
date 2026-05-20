import numpy as np
import pandas as pd
import yaml


def load_feature_config(config_path: str) -> dict:
    with open(config_path, "r") as f:
        return yaml.safe_load(f)


def build_derived_features(df: pd.DataFrame, derived_config: list) -> pd.DataFrame:
    df = df.copy()
    for feat in derived_config:
        name = feat["name"]
        expr = feat["expr"]
        try:
            df[name] = df.eval(expr)
        except Exception:
            df[name] = np.nan
    return df


def preprocess(df: pd.DataFrame, feature_config: dict) -> pd.DataFrame:
    features_cfg = feature_config.get("features", {})
    drop_cols = features_cfg.get("drop_columns", [])
    existing_drops = [c for c in drop_cols if c in df.columns]
    df = df.drop(columns=existing_drops)

    df = build_derived_features(df, features_cfg.get("derived", []))
    return df
