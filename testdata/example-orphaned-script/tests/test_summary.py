"""Tests for campaign_summary.py.

These are the tests Groundskeeper's sandbox pipeline would run inside the
ephemeral container after patching requirements.txt -- if a dependency bump
breaks something here, sandbox.Run reports Passed=False and the patch never
gets merged in the first place. If a bump instead breaks something these
tests don't cover, that's exactly the gap internal/monitor's post-merge
health watch and automatic rollback exist to catch.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from campaign_summary import (  # noqa: E402
    CampaignMetrics,
    compute_summary,
    format_summary_message,
    load_campaign_data,
)

FIXTURE_CSV = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "fixtures",
    "sample_campaign.csv",
)


@pytest.fixture
def campaign_df():
    return load_campaign_data(FIXTURE_CSV)


def test_load_campaign_data_has_expected_rows(campaign_df):
    # 4 spring_sale rows + 4 summer_launch rows in the fixture CSV.
    assert len(campaign_df) == 8
    assert set(campaign_df["campaign"]) == {"spring_sale", "summer_launch"}


def test_load_campaign_data_rejects_missing_columns(tmp_path):
    bad_csv = tmp_path / "bad.csv"
    bad_csv.write_text("campaign,clicks\nfoo,10\n")
    with pytest.raises(ValueError):
        load_campaign_data(str(bad_csv))


def test_compute_summary_aggregates_per_campaign(campaign_df):
    metrics = compute_summary(campaign_df)
    by_name = {m.campaign: m for m in metrics}

    assert set(by_name.keys()) == {"spring_sale", "summer_launch"}

    spring = by_name["spring_sale"]
    assert spring.impressions == 10000 + 25000 + 9800 + 26500
    assert spring.clicks == 450 + 900 + 410 + 980
    assert spring.conversions == 32 + 58 + 29 + 61
    assert spring.spend == pytest.approx(120.50 + 300.00 + 118.00 + 315.25)


def test_compute_summary_sorts_by_spend_descending(campaign_df):
    metrics = compute_summary(campaign_df)
    spends = [m.spend for m in metrics]
    assert spends == sorted(spends, reverse=True)


def test_campaign_metrics_derived_rates():
    m = CampaignMetrics(campaign="test", impressions=1000, clicks=100, spend=50.0, conversions=10)
    assert m.ctr == pytest.approx(0.1)
    assert m.conversion_rate == pytest.approx(0.1)
    assert m.cost_per_conversion == pytest.approx(5.0)


def test_campaign_metrics_handles_zero_denominators():
    m = CampaignMetrics(campaign="empty", impressions=0, clicks=0, spend=0.0, conversions=0)
    assert m.ctr == 0.0
    assert m.conversion_rate == 0.0
    assert m.cost_per_conversion == 0.0


def test_format_summary_message_contains_each_campaign(campaign_df):
    metrics = compute_summary(campaign_df)
    message = format_summary_message(metrics)
    assert "spring_sale" in message
    assert "summer_launch" in message
    assert "Campaign performance summary" in message


def test_format_summary_message_handles_empty_input():
    message = format_summary_message([])
    assert "no campaign data" in message.lower()
