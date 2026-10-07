"""campaign_summary.py

This is the script Groundskeeper's docs and demo walkthrough refer to as
"the analyst's cron job": a small, self-contained script an analyst wrote 18
months ago, ran nightly via cron, and then left behind when they left the
company. It reads a CSV export of ad campaign performance, computes a
summary per campaign, and posts the result to Slack.

Nobody currently at the company can safely upgrade its pinned dependencies
(see requirements.txt) if a security advisory is published against one of
them -- that's the whole reason Groundskeeper exists.

Usage:
    python campaign_summary.py path/to/campaign_export.csv

If the SLACK_WEBHOOK_URL environment variable is set, the summary is posted
there; otherwise it's printed to stdout, which is exactly what the
`tests/test_summary.py` suite (and Groundskeeper's own demo walkthrough)
rely on.
"""

from __future__ import annotations

import json
import os
import sys
from dataclasses import dataclass

import pandas as pd
import requests

REQUIRED_COLUMNS = ["campaign", "channel", "date", "impressions", "clicks", "spend", "conversions"]


@dataclass
class CampaignMetrics:
    campaign: str
    impressions: int
    clicks: int
    spend: float
    conversions: int

    @property
    def ctr(self) -> float:
        """Click-through rate, as a fraction (0.0-1.0)."""
        if self.impressions == 0:
            return 0.0
        return self.clicks / self.impressions

    @property
    def conversion_rate(self) -> float:
        """Conversions per click, as a fraction (0.0-1.0)."""
        if self.clicks == 0:
            return 0.0
        return self.conversions / self.clicks

    @property
    def cost_per_conversion(self) -> float:
        if self.conversions == 0:
            return 0.0
        return self.spend / self.conversions


def load_campaign_data(csv_path: str) -> pd.DataFrame:
    """Read the raw campaign export CSV and validate its shape.

    Raises ValueError if any required column is missing, so a malformed
    export fails loudly instead of producing a silently wrong summary.
    """
    df = pd.read_csv(csv_path)
    missing = [col for col in REQUIRED_COLUMNS if col not in df.columns]
    if missing:
        raise ValueError(f"campaign export is missing required columns: {missing}")
    return df


def compute_summary(df: pd.DataFrame) -> list[CampaignMetrics]:
    """Aggregate the raw per-row export into one CampaignMetrics per campaign."""
    grouped = df.groupby("campaign", as_index=False).agg(
        impressions=("impressions", "sum"),
        clicks=("clicks", "sum"),
        spend=("spend", "sum"),
        conversions=("conversions", "sum"),
    )

    metrics = []
    for _, row in grouped.iterrows():
        metrics.append(
            CampaignMetrics(
                campaign=str(row["campaign"]),
                impressions=int(row["impressions"]),
                clicks=int(row["clicks"]),
                spend=float(row["spend"]),
                conversions=int(row["conversions"]),
            )
        )
    # Sort by spend descending so the biggest campaigns lead the summary.
    metrics.sort(key=lambda m: m.spend, reverse=True)
    return metrics


def format_summary_message(metrics: list[CampaignMetrics]) -> str:
    """Render the computed metrics as the plain-text message posted to Slack."""
    if not metrics:
        return "Campaign summary: no campaign data found for this run."

    lines = ["*Campaign performance summary*"]
    for m in metrics:
        lines.append(
            f"- `{m.campaign}`: {m.impressions:,} impressions, "
            f"CTR {m.ctr:.2%}, conversion rate {m.conversion_rate:.2%}, "
            f"spend ${m.spend:,.2f}, cost/conversion ${m.cost_per_conversion:,.2f}"
        )
    return "\n".join(lines)


def post_to_slack(message: str, webhook_url: str | None = None) -> None:
    """Post message to Slack via an incoming webhook, or print it if no
    webhook is configured.

    This mirrors the real shape of a Slack incoming-webhook POST (see also
    Groundskeeper's own internal/notify/slack.go, which speaks the same
    payload format from the Go side for Groundskeeper's own alerts).
    """
    if not webhook_url:
        print(message)
        return

    response = requests.post(webhook_url, data=json.dumps({"text": message}), timeout=10)
    response.raise_for_status()


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} path/to/campaign_export.csv", file=sys.stderr)
        return 2

    csv_path = argv[1]
    df = load_campaign_data(csv_path)
    metrics = compute_summary(df)
    message = format_summary_message(metrics)
    post_to_slack(message, webhook_url=os.environ.get("SLACK_WEBHOOK_URL"))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
