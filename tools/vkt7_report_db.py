#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Генерация PDF-отчета ВКТ-7 непосредственно из PostgreSQL.

Используется только суточный архив:
    vkt7.daily_archive

Метаданные устройства:
    vkt7.devices

Активные элементы:
    vkt7.active_elements

Свойства элементов:
    vkt7.properties

Пример:

    python tools/vkt7_report_db.py \
        --db-url "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7" \
        --device-id 65 \
        --from 2026-09-01 \
        --to 2026-09-30 \
        --output report_2026-09.pdf

Дата --to включается в отчет.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from dataclasses import dataclass
from datetime import date, datetime, timedelta
from decimal import Decimal
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple

import psycopg2
from psycopg2.extras import RealDictCursor

from reportlab.lib import colors
from reportlab.lib.enums import TA_CENTER
from reportlab.lib.pagesizes import A4, landscape
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.platypus import (
    Paragraph,
    PageBreak,
    SimpleDocTemplate,
    Table,
    TableStyle,
)


LOG = logging.getLogger("vkt7_report_db")


# ============================================================================
# VKT-7: семантика адресов элементов
# ============================================================================
#
# Эти адреса являются адресами элементов протокола ВКТ-7.
#
# ВАЖНО:
#   active_elements говорит, какие адреса реально присутствуют у конкретного
#   прибора.
#
#   properties содержит не имена этих элементов, а свойства вроде:
#       t_dec
#       V1_dec
#       M1_dec
#       P1_dec
#       Qo1_dec
#       t_unit
#       V_unit
#       M_unit
#       P_unit
#       Qo_unit
#
# Поэтому semantic mapping нужен только для определения смысла адреса.
# Единицы и точность НЕ задаются здесь — они берутся из properties.
#
# Формат:
#   address: (имя, контур)
#
# Контур:
#   TV1
#   TV2
#
ELEMENTS: Dict[int, Tuple[str, str]] = {
    # TV1
    0: ("t1", "TV1"),
    1: ("t2", "TV1"),
    2: ("t3", "TV1"),
    3: ("V1", "TV1"),
    4: ("V2", "TV1"),
    5: ("V3", "TV1"),
    6: ("M1", "TV1"),
    7: ("M2", "TV1"),
    8: ("M3", "TV1"),
    9: ("P1", "TV1"),
    10: ("P2", "TV1"),
    11: ("Mg", "TV1"),
    12: ("Qo", "TV1"),
    13: ("Qg", "TV1"),
    14: ("dt", "TV1"),
    17: ("BNP", "TV1"),
    18: ("VOC", "TV1"),
    19: ("G1", "TV1"),
    20: ("G2", "TV1"),
    21: ("G3", "TV1"),

    # TV2
    22: ("t1", "TV2"),
    23: ("t2", "TV2"),
    24: ("t3", "TV2"),
    25: ("V1", "TV2"),
    26: ("V2", "TV2"),
    27: ("V3", "TV2"),
    28: ("M1", "TV2"),
    29: ("M2", "TV2"),
    30: ("M3", "TV2"),
    31: ("P1", "TV2"),
    32: ("P2", "TV2"),
    33: ("Mg", "TV2"),
    34: ("Qo", "TV2"),
    35: ("Qg", "TV2"),
    36: ("dt", "TV2"),
    39: ("BNP", "TV2"),
    40: ("VOC", "TV2"),
    41: ("G1", "TV2"),
    42: ("G2", "TV2"),
    43: ("G3", "TV2"),
}


# Элементы, используемые непосредственно в отчетной таблице.
#
# Слева:
#   адрес
#
# Справа:
#   заголовок PDF
#
# Семантика элемента берется из ELEMENTS, а unit/dec — из properties.
REPORT_ELEMENTS = {
    "TV1": {
        "Qo": 12,
        "M1": 6,
        "M2": 7,
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


# ============================================================================
# Модели данных
# ============================================================================

@dataclass
class ElementInfo:
    device_id: int
    address: int
    size: int

    @property
    def semantic(self) -> Optional[Tuple[str, str]]:
        return ELEMENTS.get(self.address)

    @property
    def name(self) -> Optional[str]:
        item = self.semantic
        return item[0] if item else None

    @property
    def circuit(self) -> Optional[str]:
        item = self.semantic
        return item[1] if item else None


@dataclass
class PropertyInfo:
    device_id: int
    address: int
    name: str
    value_text: Optional[str]
    numeric_value: Optional[float]
    raw: Any

    @property
    def value(self) -> Any:
        if self.numeric_value is not None:
            return self.numeric_value
        return self.value_text


# ============================================================================
# Общие функции
# ============================================================================

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
        value = value.strip()

        if not value:
            return None

        value = value.replace(",", ".")

        try:
            return float(value)
        except ValueError:
            return None

    if isinstance(value, dict):
        for key in (
            "value",
            "numeric_value",
            "raw_value",
        ):
            if key in value:
                return to_number(value[key])

    return None


def format_number(
    value: Any,
    decimals: int = 3,
) -> str:
    number = to_number(value)

    if number is None:
        return ""

    if abs(number) < 0.5 * 10 ** (-decimals):
        number = 0.0

    return f"{number:.{decimals}f}"


def sum_values(
    values: Iterable[Any],
) -> Optional[float]:
    result = []

    for value in values:
        number = to_number(value)

        if number is not None:
            result.append(number)

    if not result:
        return None

    return sum(result)


def average_values(
    values: Iterable[Any],
) -> Optional[float]:
    result = []

    for value in values:
        number = to_number(value)

        if number is not None:
            result.append(number)

    if not result:
        return None

    return sum(result) / len(result)


def paragraph(
    value: Any,
    style: ParagraphStyle,
) -> Paragraph:
    if value is None:
        value = ""

    return Paragraph(
        str(value),
        style,
    )


# ============================================================================
# Даты
# ============================================================================

def parse_date(value: str, argument_name: str) -> date:
    try:
        return date.fromisoformat(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(
            f"{argument_name}: ожидается дата YYYY-MM-DD, "
            f"получено {value!r}"
        ) from exc


def validate_period(
    first_day: date,
    last_day: date,
) -> None:
    if last_day < first_day:
        raise ValueError(
            f"Дата окончания периода ({last_day}) "
            f"раньше даты начала ({first_day})."
        )


# ============================================================================
# PostgreSQL
# ============================================================================

def connect_db(db_url: str):
    return psycopg2.connect(db_url)


def get_device(
    conn,
    device_id: int,
) -> Dict[str, Any]:
    sql = """
        SELECT
            id,
            name,
            address,
            serial_port,
            baud_rate,
            firmware_version,
            scheme_tv1,
            scheme_tv2,
            subscriber_id,
            report_day,
            model,
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
            f"Устройство device_id={device_id} "
            f"не найдено в vkt7.devices."
        )

    return dict(row)


def get_active_elements(
    conn,
    device_id: int,
) -> Dict[int, ElementInfo]:
    sql = """
        SELECT
            device_id,
            element_address,
            element_size
        FROM vkt7.active_elements
        WHERE device_id = %s
        ORDER BY element_address
    """

    result: Dict[int, ElementInfo] = {}

    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id,))

        for row in cur.fetchall():
            item = ElementInfo(
                device_id=int(row["device_id"]),
                address=int(row["element_address"]),
                size=int(row["element_size"]),
            )

            result[item.address] = item

    return result


def get_properties(
    conn,
    device_id: int,
) -> Dict[str, PropertyInfo]:
    sql = """
        SELECT
            device_id,
            element_address,
            name,
            value_text,
            numeric_value,
            raw,
            updated_at
        FROM vkt7.properties
        WHERE device_id = %s
        ORDER BY element_address
    """

    result: Dict[str, PropertyInfo] = {}

    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id,))

        for row in cur.fetchall():
            item = PropertyInfo(
                device_id=int(row["device_id"]),
                address=int(row["element_address"]),
                name=str(row["name"]),
                value_text=row["value_text"],
                numeric_value=to_number(row["numeric_value"]),
                raw=row["raw"],
            )

            result[item.name] = item

    return result


def get_daily_rows(
    conn,
    device_id: int,
    first_day: date,
    last_day: date,
) -> List[Dict[str, Any]]:
    """
    Получает ТОЛЬКО суточный архив.

    Верхняя граница исключающая, поэтому дата --to включается.
    """

    next_day = last_day + timedelta(days=1)

    sql = """
        SELECT
            device_id,
            archive_date,
            scheme_tv1,
            scheme_tv2,
            active_db,
            "values",
            quality,
            ns,
            raw,
            collected_at
        FROM vkt7.daily_archive
        WHERE device_id = %s
          AND archive_date >= %s
          AND archive_date < %s
        ORDER BY archive_date
    """

    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(
            sql,
            (
                device_id,
                first_day,
                next_day,
            ),
        )

        return [
            dict(row)
            for row in cur.fetchall()
        ]


# ============================================================================
# Работа с properties
# ============================================================================

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

    return to_number(item.value_text) or default


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
    prefix: str,
    default: int = 3,
) -> int:
    value = property_number(
        properties,
        f"{prefix}_dec",
    )

    if value is None:
        return default

    return max(0, int(value))


def unit_for(
    properties: Dict[str, PropertyInfo],
    prefix: str,
    default: str,
) -> str:
    return property_text(
        properties,
        f"{prefix}_unit",
        default,
    )


# ============================================================================
# Извлечение значения элемента из daily_archive.values
# ============================================================================

def extract_element(
    values: Any,
    element_address: int,
) -> Any:
    """
    Поддерживает варианты JSONB:

        {"12": 123.45}

        {"12": {"value": 123.45}}

        {"Qo": 123.45}

        {"TV1": {"Qo": 123.45}}

        {"TV1:Qo": 123.45}
    """

    values = normalize_json(values)

    if not isinstance(values, dict):
        return None

    name_circuit = ELEMENTS.get(element_address)

    candidates: List[Any] = [
        str(element_address),
        element_address,
    ]

    if name_circuit:
        name, circuit = name_circuit

        candidates.extend([
            name,
            f"{circuit}:{name}",
            f"{circuit}.{name}",
        ])

    for candidate in candidates:
        if candidate not in values:
            continue

        item = values[candidate]

        if isinstance(item, dict):
            for key in (
                "value",
                "numeric_value",
                "raw_value",
            ):
                if key in item:
                    return item[key]

        return item

    if name_circuit:
        name, circuit = name_circuit

        nested = values.get(circuit)

        if isinstance(nested, dict):
            for candidate in (
                name,
                f"{circuit}:{name}",
                f"{circuit}.{name}",
                str(element_address),
                element_address,
            ):
                if candidate not in nested:
                    continue

                item = nested[candidate]

                if isinstance(item, dict):
                    for key in (
                        "value",
                        "numeric_value",
                        "raw_value",
                    ):
                        if key in item:
                            return item[key]

                return item

    return None


def value_from_record(
    row: Dict[str, Any],
    element_address: int,
    active_elements: Dict[int, ElementInfo],
) -> Any:
    """
    Если элемент отсутствует в active_elements, значение не извлекаем.

    Это важно: отчет не должен показывать параметр как существующий,
    если конкретный прибор его не объявил.
    """

    if element_address not in active_elements:
        return None

    return extract_element(
        row.get("values"),
        element_address,
    )


# ============================================================================
# Шрифт
# ============================================================================

def find_font() -> str:
    """
    Ищем шрифт с кириллицей.

    Приоритет:
      1. DejaVuSans.ttf рядом со скриптом
      2. системный DejaVu Sans
      3. Liberation Sans

    Если ничего нет — Helvetica.
    """

    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    script_dir = Path(__file__).resolve().parent

    candidates = [
        script_dir / "DejaVuSans.ttf",

        Path(
            "/usr/share/fonts/truetype/dejavu/"
            "DejaVuSans.ttf"
        ),

        Path(
            "/usr/share/fonts/truetype/dejavu/"
            "DejaVuSansCondensed.ttf"
        ),

        Path(
            "/usr/share/fonts/truetype/liberation2/"
            "LiberationSans-Regular.ttf"
        ),

        Path(
            "/usr/share/fonts/truetype/liberation/"
            "LiberationSans-Regular.ttf"
        ),
    ]

    for path in candidates:
        if not path.exists():
            continue

        try:
            pdfmetrics.registerFont(
                TTFont(
                    "VKT7Font",
                    str(path),
                )
            )

            return "VKT7Font"

        except Exception as exc:
            LOG.warning(
                "Не удалось загрузить шрифт %s: %s",
                path,
                exc,
            )

    LOG.warning(
        "Шрифт с кириллицей не найден; "
        "текст PDF может отображаться некорректно."
    )

    return "Helvetica"


# ============================================================================
# PDF
# ============================================================================

def create_pdf(
    rows: List[Dict[str, Any]],
    device: Dict[str, Any],
    active_elements: Dict[int, ElementInfo],
    properties: Dict[str, PropertyInfo],
    first_day: date,
    last_day: date,
    output: Path,
) -> None:

    font_name = find_font()

    styles = getSampleStyleSheet()

    title_style = ParagraphStyle(
        "VKTTitle",
        parent=styles["Title"],
        fontName=font_name,
        fontSize=14,
        leading=16,
        alignment=TA_CENTER,
        spaceAfter=5 * mm,
    )

    header_style = ParagraphStyle(
        "VKTHeader",
        parent=styles["Normal"],
        fontName=font_name,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

    cell_style = ParagraphStyle(
        "VKTCell",
        parent=styles["Normal"],
        fontName=font_name,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

    total_style = ParagraphStyle(
        "VKTTotal",
        parent=cell_style,
        fontName=font_name,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

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

    elements = []

    # ------------------------------------------------------------------------
    # Даты
    # ------------------------------------------------------------------------

    rows_by_date: Dict[date, Dict[str, Any]] = {}

    for row in rows:
        archive_date = row["archive_date"]

        if isinstance(archive_date, datetime):
            archive_date = archive_date.date()

        elif isinstance(archive_date, str):
            archive_date = date.fromisoformat(
                archive_date[:10]
            )

        rows_by_date[archive_date] = row

    all_dates = sorted(rows_by_date)

    # ------------------------------------------------------------------------
    # Свойства точности
    # ------------------------------------------------------------------------

    tv1_t_dec = decimals_for(
        properties,
        "t",
        2,
    )

    tv1_m1_dec = decimals_for(
        properties,
        "M1",
        2,
    )

    tv1_m2_dec = decimals_for(
        properties,
        "M2",
        2,
    )

    tv1_p1_dec = decimals_for(
        properties,
        "P1",
        2,
    )

    tv1_p2_dec = decimals_for(
        properties,
        "P2",
        2,
    )

    tv1_qo_dec = decimals_for(
        properties,
        "Qo1",
        3,
    )

    tv2_t_dec = tv1_t_dec

    tv2_v1_dec = decimals_for(
        properties,
        "V1",
        2,
    )

    tv2_qo_dec = decimals_for(
        properties,
        "Qo2",
        3,
    )

    tv2_bnp_dec = decimals_for(
        properties,
        "BNP",
        2,
    )

    # ------------------------------------------------------------------------
    # Единицы
    # ------------------------------------------------------------------------

    t_unit = unit_for(
        properties,
        "t",
        "°C",
    )

    m_unit = unit_for(
        properties,
        "M",
        "т",
    )

    p_unit = unit_for(
        properties,
        "P",
        "кг/см²",
    )

    qo_unit = unit_for(
        properties,
        "Qo",
        "Гкал",
    )

    v_unit = unit_for(
        properties,
        "V",
        "м³",
    )

    bnp_unit = unit_for(
        properties,
        "BNP",
        "ч",
    )

    # ------------------------------------------------------------------------
    # Заголовок
    # ------------------------------------------------------------------------

    elements.append(
        Paragraph(
            "Название организации и номер договора",
            title_style,
        )
    )

    table_data = []

    table_data.append([
        paragraph(
            "<b>Отчет о суточных параметрах "
            "теплопотребления за период</b>",
            header_style,
        ),
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
    ])

    table_data.append([
        paragraph("Дата", header_style),

        paragraph("Отопление", header_style),
        "",
        "",
        "",
        "",
        "",
        "",
        "",

        paragraph("Горячая вода", header_style),
        "",
        "",

        paragraph(
            "Период нормальной работы, ч",
            header_style,
        ),
    ])

    table_data.append([
        "",

        paragraph(
            f"Qотопления, {qo_unit}",
            header_style,
        ),

        paragraph(
            f"Mпод, {m_unit}",
            header_style,
        ),

        paragraph(
            f"Mобр, {m_unit}",
            header_style,
        ),

        paragraph(
            f"Tпод, {t_unit}",
            header_style,
        ),

        paragraph(
            f"Tобр, {t_unit}",
            header_style,
        ),

        paragraph(
            f"ΔT, {t_unit}",
            header_style,
        ),

        paragraph(
            f"Pпод, {p_unit}",
            header_style,
        ),

        paragraph(
            f"Pобр, {p_unit}",
            header_style,
        ),

        paragraph(
            f"Qгвс, {qo_unit}",
            header_style,
        ),

        paragraph(
            f"Mгвс, {v_unit}",
            header_style,
        ),

        paragraph(
            f"Tгвс, {t_unit}",
            header_style,
        ),

        "",
    ])

    # ------------------------------------------------------------------------
    # Суточные данные
    # ------------------------------------------------------------------------

    daily_rows = []

    for current_date in all_dates:
        row = rows_by_date[current_date]

        tv1 = [
            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["Qo"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["M1"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["M2"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["t1"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["t2"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["dt"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["P1"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV1"]["P2"],
                active_elements,
            ),
        ]

        tv2 = [
            value_from_record(
                row,
                REPORT_ELEMENTS["TV2"]["Qo"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV2"]["V1"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV2"]["t1"],
                active_elements,
            ),

            value_from_record(
                row,
                REPORT_ELEMENTS["TV2"]["BNP"],
                active_elements,
            ),
        ]

        row_values = tv1 + tv2

        daily_rows.append(row_values)

        table_data.append([
            paragraph(
                current_date.strftime("%d.%m.%Y"),
                cell_style,
            )
        ] + [
            paragraph(
                format_number(
                    value,
                    decimals=(
                        tv1_qo_dec if index == 0 else
                        tv1_m1_dec if index == 1 else
                        tv1_m2_dec if index == 2 else
                        tv1_t_dec if index in (3, 4, 5) else
                        tv1_p1_dec if index == 6 else
                        tv1_p2_dec if index == 7 else
                        tv2_qo_dec if index == 8 else
                        tv2_v1_dec if index == 9 else
                        tv2_t_dec if index == 10 else
                        tv2_bnp_dec
                    ),
                ),
                cell_style,
            )
            for index, value in enumerate(row_values)
        ] + [
            paragraph(
                format_number(
                    value_from_record(
                        row,
                        REPORT_ELEMENTS["TV1"]["BNP"],
                        active_elements,
                    ),
                    decimals=tv2_bnp_dec,
                ),
                cell_style,
            )
        ])

    # ------------------------------------------------------------------------
    # ИТОГО
    # ------------------------------------------------------------------------

    sum_heating_qo = sum_values(
        row[0]
        for row in daily_rows
    )

    sum_heating_m1 = sum_values(
        row[1]
        for row in daily_rows
    )

    sum_heating_m2 = sum_values(
        row[2]
        for row in daily_rows
    )

    sum_hotwater_qo = sum_values(
        row[8]
        for row in daily_rows
    )

    sum_hotwater_v1 = sum_values(
        row[9]
        for row in daily_rows
    )

    sum_hotwater_bnp = sum_values(
        row[11]
        for row in daily_rows
    )

    total_row = [
        paragraph(
            "<b>Итого</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_qo, tv1_qo_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_m1, tv1_m1_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_m2, tv1_m2_dec)}</b>",
            total_style,
        ),

        "",
        "",
        "",
        "",

        "",

        paragraph(
            f"<b>{format_number(sum_hotwater_qo, tv2_qo_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_hotwater_v1, tv2_v1_dec)}</b>",
            total_style,
        ),

        "",

        paragraph(
            f"<b>{format_number(sum_hotwater_bnp, tv2_bnp_dec)}</b>",
            total_style,
        ),
    ]

    table_data.append(total_row)

    total_row_index = len(table_data) - 1

    # ------------------------------------------------------------------------
    # Средние значения
    # ------------------------------------------------------------------------

    avg_heating_t1 = average_values(
        row[3]
        for row in daily_rows
    )

    avg_heating_t2 = average_values(
        row[4]
        for row in daily_rows
    )

    avg_heating_dt = average_values(
        row[5]
        for row in daily_rows
    )

    avg_heating_p1 = average_values(
        row[6]
        for row in daily_rows
    )

    avg_heating_p2 = average_values(
        row[7]
        for row in daily_rows
    )

    avg_hotwater_t1 = average_values(
        row[10]
        for row in daily_rows
    )

    average_row = [
        paragraph(
            "<b>Среднее</b>",
            total_style,
        ),

        "",
        "",
        "",

        paragraph(
            f"<b>{format_number(avg_heating_t1, tv1_t_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_t2, tv1_t_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_dt, tv1_t_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_p1, tv1_p1_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_p2, tv1_p2_dec)}</b>",
            total_style,
        ),

        "",

        paragraph(
            f"<b>{format_number(avg_hotwater_t1, tv2_t_dec)}</b>",
            total_style,
        ),

        "",

        "",
    ]

    table_data.append(average_row)

    average_row_index = len(table_data) - 1

    # ------------------------------------------------------------------------
    # Таблица
    # ------------------------------------------------------------------------

    col_widths = [
        27 * mm,

        19 * mm,
        19 * mm,
        19 * mm,
        18 * mm,
        18 * mm,
        18 * mm,
        18 * mm,
        18 * mm,

        19 * mm,
        19 * mm,
        18 * mm,

        20 * mm,
    ]

    table = Table(
        table_data,
        colWidths=col_widths,
        repeatRows=3,
        hAlign="CENTER",
    )

    table.setStyle(
        TableStyle([
            (
                "GRID",
                (0, 0),
                (-1, -1),
                0.4,
                colors.black,
            ),
            (
                "VALIGN",
                (0, 0),
                (-1, -1),
                "MIDDLE",
            ),
            (
                "ALIGN",
                (0, 0),
                (-1, -1),
                "CENTER",
            ),
            (
                "BACKGROUND",
                (0, 0),
                (-1, 2),
                colors.lightgrey,
            ),
            (
                "FONTNAME",
                (0, 0),
                (-1, -1),
                font_name,
            ),

            # Общий заголовок.
            (
                "SPAN",
                (0, 0),
                (12, 0),
            ),

            # Отопление.
            (
                "SPAN",
                (1, 1),
                (8, 1),
            ),

            # Горячая вода.
            (
                "SPAN",
                (9, 1),
                (11, 1),
            ),

            # Дата.
            (
                "SPAN",
                (0, 1),
                (0, 2),
            ),

            # Нормальная работа.
            (
                "SPAN",
                (12, 1),
                (12, 2),
            ),

            # Итого.
            (
                "LINEABOVE",
                (0, total_row_index),
                (-1, total_row_index),
                1.0,
                colors.black,
            ),

            # Среднее.
            (
                "LINEABOVE",
                (0, average_row_index),
                (-1, average_row_index),
                1.0,
                colors.black,
            ),

            (
                "TOPPADDING",
                (0, 0),
                (-1, 2),
                4,
            ),

            (
                "BOTTOMPADDING",
                (0, 0),
                (-1, 2),
                4,
            ),
        ])
    )

    elements.append(table)

    # ------------------------------------------------------------------------
    # Страница 2 — холодная вода
    # ------------------------------------------------------------------------

    elements.append(PageBreak())

    elements.append(
        Paragraph(
            "Архив ВКТ-7 — холодная вода",
            title_style,
        )
    )

    cold_table_data = [
        [
            paragraph("Дата", header_style),
            paragraph(
                f"V3, {v_unit}",
                header_style,
            ),
            paragraph(
                f"BNP, {bnp_unit}",
                header_style,
            ),
        ]
    ]

    cold_rows = []

    for current_date in all_dates:
        row = rows_by_date[current_date]

        v3 = value_from_record(
            row,
            REPORT_ELEMENTS["TV2"]["V3"],
            active_elements,
        )

        bnp = value_from_record(
            row,
            REPORT_ELEMENTS["TV2"]["BNP"],
            active_elements,
        )

        cold_rows.append([
            v3,
            bnp,
        ])

        cold_table_data.append([
            paragraph(
                current_date.strftime("%d.%m.%Y"),
                cell_style,
            ),

            paragraph(
                format_number(
                    v3,
                    tv2_v1_dec,
                ),
                cell_style,
            ),

            paragraph(
                format_number(
                    bnp,
                    tv2_bnp_dec,
                ),
                cell_style,
            ),
        ])

    cold_total_v3 = sum_values(
        row[0]
        for row in cold_rows
    )

    cold_total_bnp = sum_values(
        row[1]
        for row in cold_rows
    )

    cold_total_row = [
        paragraph(
            "<b>Итого</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(cold_total_v3, tv2_v1_dec)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(cold_total_bnp, tv2_bnp_dec)}</b>",
            total_style,
        ),
    ]

    cold_table_data.append(cold_total_row)

    cold_total_row_index = len(cold_table_data) - 1

    cold_table = Table(
        cold_table_data,
        colWidths=[
            45 * mm,
            45 * mm,
            45 * mm,
        ],
        repeatRows=1,
        hAlign="CENTER",
    )

    cold_table.setStyle(
        TableStyle([
            (
                "GRID",
                (0, 0),
                (-1, -1),
                0.5,
                colors.black,
            ),
            (
                "VALIGN",
                (0, 0),
                (-1, -1),
                "MIDDLE",
            ),
            (
                "ALIGN",
                (0, 0),
                (-1, -1),
                "CENTER",
            ),
            (
                "BACKGROUND",
                (0, 0),
                (-1, 0),
                colors.lightgrey,
            ),
            (
                "FONTNAME",
                (0, 0),
                (-1, -1),
                font_name,
            ),
            (
                "LINEABOVE",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                1.0,
                colors.black,
            ),
            (
                "TOPPADDING",
                (0, 0),
                (-1, 0),
                5,
            ),
            (
                "BOTTOMPADDING",
                (0, 0),
                (-1, 0),
                5,
            ),
            (
                "TOPPADDING",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                5,
            ),
            (
                "BOTTOMPADDING",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                5,
            ),
        ])
    )

    elements.append(cold_table)

    # ------------------------------------------------------------------------
    # Информация об устройстве
    # ------------------------------------------------------------------------

    device_info = (
        f"Устройство: {device.get('name', '')}; "
        f"адрес: {device.get('address', '')}; "
        f"device_id: {device.get('id', '')}; "
        f"период: {first_day.strftime('%d.%m.%Y')} — "
        f"{last_day.strftime('%d.%m.%Y')}"
    )

    info_style = ParagraphStyle(
        "VKTInfo",
        parent=styles["Normal"],
        fontName=font_name,
        fontSize=6,
        leading=7,
        alignment=TA_CENTER,
        spaceBefore=4 * mm,
    )

    elements.append(
        Paragraph(
            device_info,
            info_style,
        )
    )

    doc.build(elements)


# ============================================================================
# CLI
# ============================================================================

def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Генерация PDF-отчета ВКТ-7 "
            "из суточного архива PostgreSQL."
        )
    )

    parser.add_argument(
        "--db-url",
        default=os.environ.get(
            "VKT7_DB_URL",
            "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7",
        ),
        help=(
            "URL подключения PostgreSQL. "
            "По умолчанию используется VKT7_DB_URL."
        ),
    )

    parser.add_argument(
        "--device-id",
        type=int,
        required=True,
        help="ID устройства из vkt7.devices.",
    )

    parser.add_argument(
        "--from",
        dest="first_day",
        type=lambda value: parse_date(
            value,
            "--from",
        ),
        required=True,
        help="Дата начала периода: YYYY-MM-DD.",
    )

    parser.add_argument(
        "--to",
        dest="last_day",
        type=lambda value: parse_date(
            value,
            "--to",
        ),
        required=True,
        help="Дата окончания периода: YYYY-MM-DD. Включительно.",
    )

    parser.add_argument(
        "--output",
        required=True,
        help="Путь к итоговому PDF.",
    )

    parser.add_argument(
        "--verbose",
        action="store_true",
        help="Включить подробный лог.",
    )

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    logging.basicConfig(
        level=(
            logging.DEBUG
            if args.verbose
            else logging.INFO
        ),
        format=(
            "%(asctime)s "
            "%(levelname)s "
            "%(message)s"
        ),
    )

    try:
        validate_period(
            args.first_day,
            args.last_day,
        )
    except ValueError as exc:
        parser.error(str(exc))

    output = Path(args.output)

    conn = None

    try:
        LOG.info(
            "Период отчета: %s — %s",
            args.first_day,
            args.last_day,
        )

        conn = connect_db(args.db_url)

        device = get_device(
            conn,
            args.device_id,
        )

        LOG.info(
            "Устройство: id=%s name=%s address=%s",
            device["id"],
            device["name"],
            device["address"],
        )

        active_elements = get_active_elements(
            conn,
            args.device_id,
        )

        LOG.info(
            "Активных элементов: %d",
            len(active_elements),
        )

        properties = get_properties(
            conn,
            args.device_id,
        )

        LOG.info(
            "Свойств: %d",
            len(properties),
        )

        rows = get_daily_rows(
            conn,
            args.device_id,
            args.first_day,
            args.last_day,
        )

        LOG.info(
            "Найдено суточных записей: %d",
            len(rows),
        )

        if not rows:
            LOG.warning(
                "За указанный период суточных записей нет."
            )

        output.parent.mkdir(
            parents=True,
            exist_ok=True,
        )

        create_pdf(
            rows=rows,
            device=device,
            active_elements=active_elements,
            properties=properties,
            first_day=args.first_day,
            last_day=args.last_day,
            output=output,
        )

        print(f"PDF: {output}")

        return 0

    except KeyboardInterrupt:
        LOG.warning(
            "Остановлено пользователем."
        )
        return 130

    except Exception as exc:
        LOG.exception(
            "Ошибка: %s",
            exc,
        )
        return 1

    finally:
        if conn is not None:
            conn.close()


if __name__ == "__main__":
    sys.exit(main())