import os
import argparse
import logging
from dotenv import load_dotenv
from data.influx_client import MarketDataFetcher

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("DataExtractor")


def main():
    parser = argparse.ArgumentParser(
        description="Extract Historical Data from InfluxDB for AI Training"
    )
    parser.add_argument(
        "--hours",
        type=int,
        default=4320,
        help="Number of historical hours to extract (e.g., 4320 for 6 months)",
    )
    parser.add_argument(
        "--output", type=str, default="data/historical_dump.csv", help="Output CSV path"
    )
    parser.add_argument(
        "--local-exchange", type=str, default="UPBIT", help="Base exchange name"
    )
    parser.add_argument(
        "--local-symbol", type=str, default="KRW-BTC", help="Base symbol"
    )
    parser.add_argument(
        "--foreign-exchange", type=str, default="COINBASE", help="Foreign exchange name"
    )
    parser.add_argument(
        "--foreign-symbol", type=str, default="BTC-USD", help="Foreign symbol"
    )

    args = parser.parse_args()

    load_dotenv()

    url = os.getenv("INFLUX_URL", "http://localhost:8086")
    token = os.getenv("INFLUX_TOKEN", "")
    org = os.getenv("INFLUX_ORG", "magnolia")
    bucket = os.getenv("INFLUX_BUCKET", "market_data")

    if not token:
        logger.error(
            "INFLUX_TOKEN environment variable is not set. Please check your .env file."
        )
        return

    logger.info(f"Connecting to InfluxDB at {url} (Bucket: {bucket})")
    fetcher = MarketDataFetcher(url=url, token=token, org=org, bucket=bucket)

    logger.info(
        f"Fetching last {args.hours} hours of data for {args.local_symbol} vs {args.foreign_symbol}..."
    )
    df = fetcher.fetch_recent_window(
        local_exchange=args.local_exchange,
        local_symbol=args.local_symbol,
        local_is_future=False,
        foreign_exchange=args.foreign_exchange,
        foreign_symbol=args.foreign_symbol,
        foreign_is_future=False,
        window_hours=args.hours,
    )

    if df is not None and not df.empty:
        os.makedirs(os.path.dirname(args.output), exist_ok=True)
        df.to_csv(args.output, index=False)
        logger.info(f"Successfully extracted {len(df)} rows to {args.output}")
    else:
        logger.error(
            "Failed to extract data or no data found in the specified time window."
        )


if __name__ == "__main__":
    main()
