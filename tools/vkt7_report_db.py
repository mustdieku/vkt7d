#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Generate a PDF report from VKT-7 daily archives in PostgreSQL.

Daily archive is used for daily increments. Total archive is used as
an absolute cumulative anchor:
    vkt7.daily_archive
    vkt7.total_archive

Element availability is taken from:
    vkt7.active_elements

Units and decimal precision are taken from:
    vkt7.properties

Example:
    python tools/vkt7_report_db.py \
        --db-url "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7" \
        --device-id 65 \
        --from 2026-09-01 \
        --to 2026-09-30 \
        --output report_2026-09.pdf

The --to date is inclusive.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
from dataclasses import dataclass
from datetime import date, datetime, timedelta
from decimal import Decimal
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional

import psycopg2
from psycopg2.extras import RealDictCursor
from reportlab.lib import colors
from reportlab.lib.enums import TA_CENTER
from reportlab.lib.pagesizes import A4, landscape
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.platypus import PageBreak, Paragraph, SimpleDocTemplate, Table, TableStyle


LOG = logging.getLogger("vkt7_report_db")


# -----------------------------------------------------------------------------
# VKT-7 element addresses
# -----------------------------------------------------------------------------
# active_elements contains protocol addresses.  daily_archive.values does NOT
# use these numeric addresses as JSON keys.  It stores semantic archive names:
#     t1_1, t2_1, V1_1, M1_1, Qo_1, ...
#     t1_2, t2_2, V1_2, M1_2, Qo_2, ...
# Keep this mapping explicit; it is the important part of archive extraction.

ARCHIVE_NAMES: Dict[int, str] = {
    # TV1
    0: "t1_1",
    1: "t2_1",
    2: "t3_1",
    3: "V1_1",
    4: "V2_1",
    5: "V3_1",
    6: "M1_1",
    7: "M2_1",
    8: "M3_1",
    9: "P1_1",
    10: "P2_1",
    11: "Mg_1",
    12: "Qo_1",
    13: "Qg_1",
    14: "dt_1",
    17: "BNP_1",
    18: "VOC_1",
    19: "G1_1",
    20: "G2_1",
    21: "G3_1",
    # TV2
    22: "t1_2",
    23: "t2_2",
    24: "t3_2",
    25: "V1_2",
    26: "V2_2",
    27: "V3_2",
    28: "M1_2",
    29: "M2_2",
    30: "M3_2",
    31: "P1_2",
    32: "P2_2",
    33: "Mg_2",
    34: "Qo_2",
    35: "Qg_2",
    36: "dt_2",
    39: "BNP_2",
    40: "VOC_2",
    41: "G1_2",
    42: "G2_2",
    43: "G3_2",
}

# Elements actually printed in the report.
REPORT_ELEMENTS = {
    "TV1": {
        "Qo": 12,
        "V1": 3,
        "V2": 4,
        "t1": 0,
        "t2": 1,
        "dt": 14,
        "P1": 9,
        "P2": 10,
        "BNP": 17,
    },
    "TV2": {
        "Qo": 34,
        "V1": 25,
        "t1": 22,
        "BNP": 39,
        "V3": 27,
    },
}


@dataclass
class PropertyInfo:
    name: str
    value_text: Optional[str]
    numeric_value: Optional[float]


# -----------------------------------------------------------------------------
# General helpers
# -----------------------------------------------------------------------------

def normalize_json(value: Any) -> Any:
    if value is None:
        return None
    if isinstance(value, (dict, list, int, float, bool)):
        return value
    if isinstance(value, Decimal):
        return float(value)
    if isinstance(value, str):
        value = value.strip()
        if not value:
            return None
        try:
            return json.loads(value)
        except (TypeError, ValueError, json.JSONDecodeError):
            return value
    return value


def to_number(value: Any) -> Optional[float]:
    if value is None:
        return None
    if isinstance(value, bool):
        return float(value)
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, Decimal):
        return float(value)
    if isinstance(value, str):
        value = value.strip().replace(",", ".")
        if not value:
            return None
        try:
            return float(value)
        except ValueError:
            return None
    if isinstance(value, dict):
        for key in ("value", "numeric_value", "raw_value"):
            if key in value:
                return to_number(value[key])
    return None


def format_number(value: Any, decimals: int = 3) -> str:
    number = to_number(value)
    if number is None:
        return ""
    number /= 10 ** decimals
    return f"{number:.{decimals}f}"


def sum_values(values: Iterable[Any]) -> Optional[float]:
    numbers = [n for n in (to_number(v) for v in values) if n is not None]
    return sum(numbers) if numbers else None


def average_values(values: Iterable[Any]) -> Optional[float]:
    numbers = [n for n in (to_number(v) for v in values) if n is not None]
    return sum(numbers) / len(numbers) if numbers else None


def paragraph(value: Any, style: ParagraphStyle) -> Paragraph:
    return Paragraph("" if value is None else str(value), style)


# -----------------------------------------------------------------------------
# Dates
# -----------------------------------------------------------------------------

def parse_date(value: str, argument_name: str) -> date:
    try:
        return date.fromisoformat(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(
            f"{argument_name}: ожидается дата YYYY-MM-DD, получено {value!r}"
        ) from exc


def validate_period(first_day: date, last_day: date) -> None:
    if last_day < first_day:
        raise ValueError(
            f"Дата окончания периода ({last_day}) раньше даты начала ({first_day})."
        )


# -----------------------------------------------------------------------------
# PostgreSQL
# -----------------------------------------------------------------------------

def connect_db(db_url: str):
    return psycopg2.connect(db_url)


def get_device(conn, device_id: int) -> Dict[str, Any]:
    sql = """
        SELECT
            id,
            name,
            address,
            model,
            scheme_tv1,
            scheme_tv2,
            subscriber_id,
            report_day,
            active_db,
            last_seen_at
        FROM vkt7.devices
        WHERE id = %s
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id,))
        row = cur.fetchone()
    if row is None:
        raise RuntimeError(
            f"Устройство device_id={device_id} не найдено в vkt7.devices."
        )
    return dict(row)


def get_active_elements(conn, device_id: int) -> set[int]:
    sql = """
        SELECT element_address
        FROM vkt7.active_elements
        WHERE device_id = %s
        ORDER BY element_address
    """
    with conn.cursor() as cur:
        cur.execute(sql, (device_id,))
        return {int(row[0]) for row in cur.fetchall()}


def get_properties(conn, device_id: int) -> Dict[str, PropertyInfo]:
    sql = """
        SELECT
            name,
            value_text,
            numeric_value
        FROM vkt7.properties
        WHERE device_id = %s
        ORDER BY element_address
    """
    result: Dict[str, PropertyInfo] = {}
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id,))
        for row in cur.fetchall():
            result[str(row["name"])] = PropertyInfo(
                name=str(row["name"]),
                value_text=row["value_text"],
                numeric_value=to_number(row["numeric_value"]),
            )
    return result


def get_daily_rows(
    conn,
    device_id: int,
    first_day: date,
    last_day: date,
) -> List[Dict[str, Any]]:
    """Read only vkt7.daily_archive; --to is inclusive."""
    next_day = last_day + timedelta(days=1)
    sql = """
        SELECT
            archive_date,
            "values"
        FROM vkt7.daily_archive
        WHERE device_id = %s
          AND archive_date >= %s
          AND archive_date < %s
        ORDER BY archive_date
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id, first_day, next_day))
        return [dict(row) for row in cur.fetchall()]


def get_total_rows(
    conn,
    device_id: int,
    first_day: date,
    last_day: date,
) -> List[Dict[str, Any]]:
    """Read total-archive records around the requested period.

    The total archive contains absolute cumulative values. At least one
    record before/inside/after the report period is required to reconstruct
    cumulative values for daily rows.

    Two records are fetched on each side so that the report remains usable
    even when there are gaps in the total archive.
    """
    sql = """
        (
            SELECT
                archive_date,
                "values"
            FROM vkt7.total_archive
            WHERE device_id = %s
              AND archive_date < %s
            ORDER BY archive_date DESC
            LIMIT 2
        )
        UNION ALL
        (
            SELECT
                archive_date,
                "values"
            FROM vkt7.total_archive
            WHERE device_id = %s
              AND archive_date >= %s
              AND archive_date <= %s
            ORDER BY archive_date
        )
        UNION ALL
        (
            SELECT
                archive_date,
                "values"
            FROM vkt7.total_archive
            WHERE device_id = %s
              AND archive_date > %s
            ORDER BY archive_date
            LIMIT 2
        )
        ORDER BY archive_date
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(
            sql,
            (
                device_id,
                first_day,
                device_id,
                first_day,
                last_day,
                device_id,
                last_day,
            ),
        )
        return [dict(row) for row in cur.fetchall()]


# -----------------------------------------------------------------------------
# Properties
# -----------------------------------------------------------------------------

def property_number(
    properties: Dict[str, PropertyInfo],
    name: str,
    default: Optional[float] = None,
) -> Optional[float]:
    item = properties.get(name)
    if item is None:
        return default
    if item.numeric_value is not None:
        return item.numeric_value
    text_value = to_number(item.value_text)
    return text_value if text_value is not None else default


def property_text(
    properties: Dict[str, PropertyInfo],
    name: str,
    default: str = "",
) -> str:
    item = properties.get(name)
    if item is None:
        return default
    if item.value_text is not None:
        return str(item.value_text)
    if item.numeric_value is not None:
        return str(item.numeric_value)
    return default


def decimals_for(
    properties: Dict[str, PropertyInfo],
    name: str,
    default: int,
) -> int:
    value = property_number(properties, f"{name}_dec")
    return max(0, int(value)) if value is not None else default


def unit_for(
    properties: Dict[str, PropertyInfo],
    name: str,
    default: str,
) -> str:
    return property_text(properties, f"{name}_unit", default)


def decimals_for_archive(
    properties: Dict[str, PropertyInfo],
    archive_name: str,
    default: int,
) -> int:
    """Return the _dec precision applicable to an archive JSON key.

    Examples:
        t1_1  -> t_dec
        M1_2  -> M1_dec
        Qo_1  -> Qo1_dec
        Qo_2  -> Qo2_dec
        V1_2  -> V1_dec
        P2_1  -> P2_dec
        BNP_2 -> BNP_dec (if present)

    The TV suffix (_1/_2) identifies the thermal circuit and is not part
    of the property name.  Qo is the only reported quantity whose property
    name additionally contains the TV number.
    """
    if archive_name.endswith("_1"):
        base = archive_name[:-2]
        tv = 1
    elif archive_name.endswith("_2"):
        base = archive_name[:-2]
        tv = 2
    else:
        base = archive_name
        tv = None

    if base == "Qo":
        property_name = f"Qo{tv}_dec" if tv is not None else "Qo_dec"
    else:
        property_name = f"{base}_dec"

    return decimals_for(properties, property_name[:-4], default)


def decimals_for_element(
    properties: Dict[str, PropertyInfo],
    element_address: int,
    default: int,
) -> int:
    archive_name = ARCHIVE_NAMES.get(element_address)
    if archive_name is None:
        return default
    return decimals_for_archive(properties, archive_name, default)


# -----------------------------------------------------------------------------
# Archive value extraction
# -----------------------------------------------------------------------------

def extract_element(
    values: Any,
    element_address: int,
    active_elements: set[int],
) -> Any:
    """Get one element from daily_archive.values.

    active_elements is the availability filter.
    ARCHIVE_NAMES maps protocol address -> actual JSON key.
    """
    if element_address not in active_elements:
        return None

    archive_name = ARCHIVE_NAMES.get(element_address)
    if archive_name is None:
        return None

    values = normalize_json(values)
    if not isinstance(values, dict):
        return None

    item = values.get(archive_name)
    if isinstance(item, dict):
        for key in ("value", "numeric_value", "raw_value"):
            if key in item:
                return item[key]
    return item


def value_from_record(
    row: Dict[str, Any],
    element_address: int,
    active_elements: set[int],
) -> Any:
    return extract_element(row.get("values"), element_address, active_elements)


def archive_date_value(
    row: Dict[str, Any],
    archive_name: str,
) -> Optional[float]:
    """Return a numeric value from an archive JSON object.

    Total archive values are stored using the same semantic JSON keys as
    daily archive values, therefore no protocol address conversion is
    required here.
    """
    values = normalize_json(row.get("values"))
    if not isinstance(values, dict):
        return None
    return to_number(values.get(archive_name))


def parse_archive_date(value: Any) -> date:
    if isinstance(value, datetime):
        return value.date()
    if isinstance(value, date):
        return value
    return date.fromisoformat(str(value)[:10])


def build_cumulative_values(
    daily_rows: List[Dict[str, Any]],
    total_rows: List[Dict[str, Any]],
    archive_name: str,
) -> Dict[date, Optional[float]]:
    """Reconstruct absolute cumulative values for daily archive rows.

    A total-archive value is an absolute meter reading. Daily archive
    values are increments for a complete day.

    If an anchor exists before the report period:

        cumulative(day) =
            total(anchor) +
            sum(daily(anchor + 1 .. day))

    If only an anchor after the period exists:

        cumulative(day) =
            total(anchor) -
            sum(daily(day + 1 .. anchor))

    If the anchor is inside the report period, values are reconstructed
    in both directions from that anchor.

    total_archive is used as the absolute reference. monthly_archive is
    deliberately not used because its values describe a period rather
    than an absolute cumulative meter reading.
    """
    if not daily_rows or not total_rows:
        return {}

    daily: Dict[date, Optional[float]] = {}
    for row in daily_rows:
        daily[parse_archive_date(row["archive_date"])] = archive_date_value(
            row,
            archive_name,
        )

    totals: List[tuple[date, float]] = []
    for row in total_rows:
        d = parse_archive_date(row["archive_date"])
        value = archive_date_value(row, archive_name)
        if value is not None:
            totals.append((d, value))

    if not totals:
        return {}

    totals.sort()
    report_dates = sorted(daily)
    first_day = report_dates[0]
    last_day = report_dates[-1]

    # Select the anchor closest to the report period.
    #
    # Important: total_archive may contain a record INSIDE the requested
    # period (for example 2026-09-30 for a report 2026-09-19..2026-10-19).
    # Such a record must be used as the reference point.
    inside = [item for item in totals if first_day <= item[0] <= last_day]

    if inside:
        # Prefer the latest anchor inside the period.  This minimizes the
        # amount of backward reconstruction and is useful when daily
        # archive has gaps near the beginning of the report.
        anchor_date, anchor_value = inside[-1]
    else:
        before = [item for item in totals if item[0] < first_day]
        after = [item for item in totals if item[0] > last_day]

        if before:
            # Normal case: reconstruct forward from the latest absolute
            # total before the report.
            anchor_date, anchor_value = before[-1]
        elif after:
            # Fallback: reconstruct backwards from the earliest absolute
            # total after the report.
            anchor_date, anchor_value = after[0]
        else:
            LOG.warning(
                "No usable total_archive anchor for %s in period %s..%s",
                archive_name,
                first_day,
                last_day,
            )
            return {}

    result: Dict[date, Optional[float]] = {}

    # The anchor itself is an absolute cumulative value.
    if first_day <= anchor_date <= last_day:
        result[anchor_date] = anchor_value

        # Reconstruct forward from the anchor:
        #
        # C(day) = C(anchor) + daily(anchor+1 .. day)
        running = anchor_value
        for d in report_dates:
            if d <= anchor_date:
                continue

            increment = daily.get(d)
            if increment is None:
                result[d] = None
                continue

            running += increment
            result[d] = running

        # Reconstruct backwards from the anchor:
        #
        # C(day) = C(anchor) - daily(day+1 .. anchor)
        running = anchor_value
        for d in reversed(report_dates):
            if d >= anchor_date:
                continue

            increment = daily.get(d + timedelta(days=1))
            if increment is None:
                result[d] = None
                continue

            running -= increment
            result[d] = running

    elif anchor_date < first_day:
        # Anchor before the report: reconstruct forward.
        running = anchor_value

        for d in report_dates:
            increment = daily.get(d)
            if increment is None:
                result[d] = None
                continue

            running += increment
            result[d] = running

    else:
        # Anchor after the report: reconstruct backwards.
        running = anchor_value

        for d in reversed(report_dates):
            increment = daily.get(d + timedelta(days=1))
            if increment is None:
                result[d] = None
                continue

            running -= increment
            result[d] = running

    LOG.info(
        "Cumulative %s reconstructed from total_archive anchor %s = %s",
        archive_name,
        anchor_date,
        anchor_value,
    )
    return result


# -----------------------------------------------------------------------------
# PDF
# -----------------------------------------------------------------------------

def find_font() -> str:
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    script_dir = Path(__file__).resolve().parent
    candidates = [
        script_dir / "DejaVuSans.ttf",
        Path("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"),
        Path("/usr/share/fonts/truetype/dejavu/DejaVuSansCondensed.ttf"),
        Path("/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf"),
        Path("/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf"),
    ]

    for path in candidates:
        if not path.exists():
            continue
        try:
            pdfmetrics.registerFont(TTFont("VKT7Font", str(path)))
            return "VKT7Font"
        except Exception as exc:
            LOG.warning("Не удалось загрузить шрифт %s: %s", path, exc)

    LOG.warning("Шрифт с кириллицей не найден; PDF может отображаться некорректно.")
    return "Helvetica"


def create_pdf(
    rows: List[Dict[str, Any]],
    total_rows: List[Dict[str, Any]],    device: Dict[str, Any],
    active_elements: set[int],
    properties: Dict[str, PropertyInfo],
    first_day: date,
    last_day: date,
    output: Path,
) -> None:
    font_name = find_font()
    styles = getSampleStyleSheet()

    title_style = ParagraphStyle(
        "VKTTitle", parent=styles["Title"], fontName=font_name,
        fontSize=14, leading=16, alignment=TA_CENTER, spaceAfter=5 * mm,
    )
    header_style = ParagraphStyle(
        "VKTHeader", parent=styles["Normal"], fontName=font_name,
        fontSize=7, leading=8, alignment=TA_CENTER,
    )
    cell_style = ParagraphStyle(
        "VKTCell", parent=styles["Normal"], fontName=font_name,
        fontSize=7, leading=8, alignment=TA_CENTER,
    )
    total_style = ParagraphStyle(
        "VKTTotal", parent=cell_style, fontName=font_name,
        fontSize=7, leading=8, alignment=TA_CENTER,
    )
    info_style = ParagraphStyle(
        "VKTInfo", parent=styles["Normal"], fontName=font_name,
        fontSize=6, leading=7, alignment=TA_CENTER, spaceBefore=4 * mm,
    )

    # Precision and units are taken from properties.
    # The property name is derived from the actual archive key, so V1_2
    # uses V1_dec (not V2_dec), Qo_2 uses Qo2_dec, etc.
    tv1_qo_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["Qo"], 3)
    tv1_v1_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["V1"], 2)
    tv1_v2_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["V2"], 2)
    tv1_t1_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["t1"], 2)
    tv1_t2_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["t2"], 2)
    tv1_dt_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["dt"], tv1_t1_dec)
    tv1_p1_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["P1"], 2)
    tv1_p2_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["P2"], 2)
    tv1_bnp_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV1"]["BNP"], 0)

    tv2_qo_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV2"]["Qo"], 3)
    tv2_v1_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV2"]["V1"], 2)
    tv2_t1_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV2"]["t1"], 2)
    tv2_bnp_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV2"]["BNP"], 0)
    tv2_v3_dec = decimals_for_element(properties, REPORT_ELEMENTS["TV2"]["V3"], 2)

    t_unit = unit_for(properties, "t", "°C")
    m_unit = unit_for(properties, "M", "т")
    p_unit = unit_for(properties, "P", "кг/см²")
    qo_unit = unit_for(properties, "Qo", "Гкал")
    v_unit = unit_for(properties, "V", "м³")
    bnp_unit = unit_for(properties, "BNP", "ч")

    doc = SimpleDocTemplate(
        str(output),
        pagesize=landscape(A4),
        rightMargin=8 * mm,
        leftMargin=8 * mm,
        topMargin=8 * mm,
        bottomMargin=8 * mm,
        title="Архив ВКТ-7",
        author="vkt7_report_db.py",
    )

    # One archive row per date.
    rows_by_date: Dict[date, Dict[str, Any]] = {}
    for row in rows:
        d = row["archive_date"]
        if isinstance(d, datetime):
            d = d.date()
        elif isinstance(d, str):
            d = date.fromisoformat(d[:10])
        rows_by_date[d] = row
    all_dates = sorted(rows_by_date)

    # V3_2 is the cold-water volume shown on page 2.
    # Its daily archive value is an increment, while total_archive.V3_2
    # is an absolute cumulative value.
    cumulative_v3 = build_cumulative_values(
        rows,
        total_rows,
        "V3_2",
    )

    elements = [Paragraph("Название организации и номер договора", title_style)]

    # ------------------------------------------------------------------ page 1
    table_data = [
        [paragraph(f"<b>Отчет о суточных параметрах теплопотребления за период: {first_day.strftime('%d.%m.%Y')} — {last_day.strftime('%d.%m.%Y')}</b>", header_style)] + [""] * 12,
        [
            paragraph("Дата", header_style),
            paragraph("Отопление", header_style), "", "", "", "", "", "", "",
            paragraph("Горячая вода", header_style), "", "",
            paragraph("Период нормальной работы, ч", header_style),
        ],
        [
            "",
            paragraph(f"Qотопления, {qo_unit}", header_style),
            paragraph(f"Vпод, {v_unit}", header_style),
            paragraph(f"Vобр, {v_unit}", header_style),
            paragraph(f"Tпод, {t_unit}", header_style),
            paragraph(f"Tобр, {t_unit}", header_style),
            paragraph(f"ΔT, {t_unit}", header_style),
            paragraph(f"Pпод, {p_unit}", header_style),
            paragraph(f"Pобр, {p_unit}", header_style),
            paragraph(f"Qгвс, {qo_unit}", header_style),
            paragraph(f"Vгвс, {v_unit}", header_style),
            paragraph(f"Tгвс, {t_unit}", header_style),
            "",
        ],
    ]

    daily_rows: List[List[Any]] = []
    for current_date in all_dates:
        row = rows_by_date[current_date]
        tv1 = [
            value_from_record(row, REPORT_ELEMENTS["TV1"]["Qo"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["V1"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["V2"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["t1"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["t2"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["dt"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["P1"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV1"]["P2"], active_elements),
        ]
        tv2 = [
            value_from_record(row, REPORT_ELEMENTS["TV2"]["Qo"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV2"]["V1"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV2"]["t1"], active_elements),
            value_from_record(row, REPORT_ELEMENTS["TV2"]["BNP"], active_elements),
        ]
        values = tv1 + tv2
        daily_rows.append(values)

        decimals = [
            tv1_qo_dec, tv1_v1_dec, tv1_v2_dec, tv1_t1_dec, tv1_t2_dec,
            tv1_dt_dec, tv1_p1_dec, tv1_p2_dec, tv2_qo_dec, tv2_v1_dec,
            tv2_t1_dec, tv2_bnp_dec,
        ]
        table_data.append(
            [paragraph(current_date.strftime("%d.%m.%Y"), cell_style)]
            + [paragraph(format_number(v, d), cell_style) for v, d in zip(values, decimals)]
        )

    # Totals.
    total_row = [paragraph("<b>Итого</b>", total_style)]
    total_row += [
        paragraph(f"<b>{format_number(sum_values(r[0] for r in daily_rows), tv1_qo_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(sum_values(r[1] for r in daily_rows), tv1_v1_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(sum_values(r[2] for r in daily_rows), tv1_v2_dec)}</b>", total_style),
        "", "", "", "", "",
        paragraph(f"<b>{format_number(sum_values(r[8] for r in daily_rows), tv2_qo_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(sum_values(r[9] for r in daily_rows), tv2_v1_dec)}</b>", total_style),
        "",
        paragraph(f"<b>{format_number(sum_values(r[11] for r in daily_rows), tv1_bnp_dec)}</b>", total_style),
    ]
    table_data.append(total_row)
    total_row_index = len(table_data) - 1

    # Averages.
    average_row = [paragraph("<b>Среднее</b>", total_style)]
    average_row += [
        "", "", "",
        paragraph(f"<b>{format_number(average_values(r[3] for r in daily_rows), tv1_t1_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(average_values(r[4] for r in daily_rows), tv1_t2_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(average_values(r[5] for r in daily_rows), tv1_dt_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(average_values(r[6] for r in daily_rows), tv1_p1_dec)}</b>", total_style),
        paragraph(f"<b>{format_number(average_values(r[7] for r in daily_rows), tv1_p2_dec)}</b>", total_style),
        "","",
        paragraph(f"<b>{format_number(average_values(r[10] for r in daily_rows), tv2_t1_dec)}</b>", total_style),
        "",
    ]
    table_data.append(average_row)
    average_row_index = len(table_data) - 1

    col_widths = [27 * mm, 19 * mm, 19 * mm, 19 * mm, 18 * mm, 18 * mm,
                  18 * mm, 18 * mm, 18 * mm, 19 * mm, 19 * mm, 18 * mm, 20 * mm]
    table = Table(table_data, colWidths=col_widths, repeatRows=3, hAlign="CENTER")
    table.setStyle(TableStyle([
        ("GRID", (0, 0), (-1, -1), 0.4, colors.black),
        ("VALIGN", (0, 0), (-1, -1), "MIDDLE"),
        ("ALIGN", (0, 0), (-1, -1), "CENTER"),
        ("BACKGROUND", (0, 0), (-1, 2), colors.lightgrey),
        ("FONTNAME", (0, 0), (-1, -1), font_name),
        ("SPAN", (0, 0), (12, 0)),
        ("SPAN", (1, 1), (8, 1)),
        ("SPAN", (9, 1), (11, 1)),
        ("SPAN", (0, 1), (0, 2)),
        ("SPAN", (12, 1), (12, 2)),
        ("LINEABOVE", (0, total_row_index), (-1, total_row_index), 1.0, colors.black),
        ("LINEABOVE", (0, average_row_index), (-1, average_row_index), 1.0, colors.black),
        ("TOPPADDING", (0, 0), (-1, 2), 4),
        ("BOTTOMPADDING", (0, 0), (-1, 2), 4),
    ]))
    elements.append(table)

    # ------------------------------------------------------------------ page 2
    elements.append(PageBreak())
    elements.append(Paragraph("Название организации и номер договора", title_style))

    cold_table_data = [
        [paragraph(f"<b>Отчет о суточных параметрах потребления воды за период: {first_day.strftime('%d.%m.%Y')} — {last_day.strftime('%d.%m.%Y')}</b>", header_style)] + [""] * 3,
        [
            paragraph("Дата", header_style),
            paragraph(f"Vхвс, {v_unit}", header_style),
            paragraph(f"Накоплено Vхвс, {v_unit}", header_style),
            paragraph(f"Период нормальной работы, {bnp_unit}", header_style),
        ]
    ]
    cold_rows: List[List[Any]] = []

    for current_date in all_dates:
        row = rows_by_date[current_date]
        v3 = value_from_record(row, REPORT_ELEMENTS["TV2"]["V3"], active_elements)
        bnp = value_from_record(row, REPORT_ELEMENTS["TV2"]["BNP"], active_elements)
        cumulative = cumulative_v3.get(current_date)
        cold_rows.append([v3, cumulative, bnp])
        cold_table_data.append([
            paragraph(current_date.strftime("%d.%m.%Y"), cell_style),
            paragraph(format_number(v3, tv2_v3_dec), cell_style),
            paragraph(format_number(cumulative, tv2_v3_dec), cell_style),
            paragraph(format_number(bnp, tv2_bnp_dec), cell_style),
        ])

    cold_total_row = [
        paragraph("<b>Итого</b>", total_style),
        paragraph(f"<b>{format_number(sum_values(r[0] for r in cold_rows), tv2_v3_dec)}</b>", total_style),
        "",
        paragraph(f"<b>{format_number(sum_values(r[2] for r in cold_rows), tv2_bnp_dec)}</b>", total_style),
    ]
    cold_table_data.append(cold_total_row)
    cold_total_row_index = len(cold_table_data) - 1

    cold_table = Table(cold_table_data, colWidths=[35 * mm, 35 * mm, 45 * mm, 35 * mm], repeatRows=1, hAlign="CENTER")
    cold_table.setStyle(TableStyle([
        ("GRID", (0, 0), (-1, -1), 0.5, colors.black),
        ("VALIGN", (0, 0), (-1, -1), "MIDDLE"),
        ("ALIGN", (0, 0), (-1, -1), "CENTER"),
        ("BACKGROUND", (0, 0), (-1, 1), colors.lightgrey),
        ("FONTNAME", (0, 0), (-1, -1), font_name),
        ("SPAN", (0, 0), (3, 0)),
        ("LINEABOVE", (0, cold_total_row_index), (-1, cold_total_row_index), 1.0, colors.black),
        ("TOPPADDING", (0, 0), (-1, 0), 5),
        ("BOTTOMPADDING", (0, 0), (-1, 0), 5),
        ("TOPPADDING", (0, cold_total_row_index), (-1, cold_total_row_index), 5),
        ("BOTTOMPADDING", (0, cold_total_row_index), (-1, cold_total_row_index), 5),
     ]))
    elements.append(cold_table)

    device_info = (
        f"Устройство: {device.get('name', '')}; "
        f"адрес: {device.get('address', '')}; "
        f"device_id: {device.get('id', '')}; "
        f"период: {first_day.strftime('%d.%m.%Y')} — {last_day.strftime('%d.%m.%Y')}"
    )
    elements.append(Paragraph(device_info, info_style))

    doc.build(elements)


# -----------------------------------------------------------------------------
# CLI
# -----------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Генерация PDF-отчета ВКТ-7 из суточного и итогового архивов PostgreSQL."
        )
    )
    parser.add_argument(
        "--db-url",
        default=os.environ.get("VKT7_DB_URL", "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7"),
        help="URL подключения PostgreSQL. Можно задать через VKT7_DB_URL.",
    )
    parser.add_argument("--device-id", type=int, required=True, help="ID устройства из vkt7.devices.")
    parser.add_argument(
        "--from", dest="first_day", type=lambda v: parse_date(v, "--from"),
        required=True, help="Дата начала периода: YYYY-MM-DD.",
    )
    parser.add_argument(
        "--to", dest="last_day", type=lambda v: parse_date(v, "--to"),
        required=True, help="Дата окончания периода: YYYY-MM-DD. Включительно.",
    )
    parser.add_argument("--output", required=True, help="Путь к итоговому PDF.")
    parser.add_argument("--verbose", action="store_true", help="Включить подробный лог.")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
    )

    try:
        validate_period(args.first_day, args.last_day)
    except ValueError as exc:
        parser.error(str(exc))

    output = Path(args.output)
    conn = None

    try:
        LOG.info("Период отчета: %s — %s", args.first_day, args.last_day)
        conn = connect_db(args.db_url)

        device = get_device(conn, args.device_id)
        active_elements = get_active_elements(conn, args.device_id)
        properties = get_properties(conn, args.device_id)
        rows = get_daily_rows(conn, args.device_id, args.first_day, args.last_day)
        total_rows = get_total_rows(conn, args.device_id, args.first_day, args.last_day)

        LOG.info(
            "Устройство: id=%s name=%s address=%s",
            device["id"], device.get("name"), device.get("address"),
        )
        LOG.info("Активных элементов: %d", len(active_elements))
        LOG.info("Свойств: %d", len(properties))
        LOG.info("Найдено суточных записей: %d", len(rows))
        LOG.info("Найдено итоговых записей: %d", len(total_rows))

        if not rows:
            LOG.warning("За указанный период суточных записей нет.")

        output.parent.mkdir(parents=True, exist_ok=True)
        create_pdf(
            rows=rows,
            device=device,
            active_elements=active_elements,
            properties=properties,
            first_day=args.first_day,
            last_day=args.last_day,
            total_rows=total_rows,
            output=output,
        )
        LOG.info("PDF создан: %s", output)
        return 0

    except Exception as exc:
        LOG.exception("Ошибка генерации отчета: %s", exc)
        return 1
    finally:
        if conn is not None:
            conn.close()


if __name__ == "__main__":
    raise SystemExit(main())
