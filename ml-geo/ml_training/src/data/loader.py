import os
import glob
import pandas as pd


def load_raw_data(data_dir: str) -> pd.DataFrame:
    csv_files = glob.glob(os.path.join(data_dir, "*.csv"))
    if not csv_files:
        raise FileNotFoundError(f"No CSV files found in {data_dir}")

    frames = []
    for f in sorted(csv_files):
        df = pd.read_csv(f)
        frames.append(df)

    data = pd.concat(frames, ignore_index=True)
    data = data.sort_values("timestamp").reset_index(drop=True)
    return data
