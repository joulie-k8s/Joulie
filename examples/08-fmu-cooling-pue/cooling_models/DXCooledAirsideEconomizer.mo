model DXCooledAirsideEconomizer
  "Physics-based DX-cooled data center with airside economizer for FMU co-simulation.

   Captures the key dynamics of Buildings.Applications.DataCenters.DXCooled:
     - Variable-speed DX compressor with temperature-dependent COP
     - Airside economizer (free cooling when outdoor temp permits)
     - Three cooling modes: free cooling, partial mechanical, full mechanical
     - Room thermal mass and air mixing
     - Fan power scaling with affinity laws (cubic speed relation)

   Designed for robust FMU export with external IT load and weather inputs.
   Reference: LBL Buildings Library v12.1.0
              Buildings.Applications.DataCenters.DXCooled.Examples.DXCooledAirsideEconomizer
  "

  // ===== External Inputs =====
  Modelica.Blocks.Interfaces.RealInput Q_IT(unit="W")
    "Total IT heat load in Watts (replaces fixed QRooInt_flow)"
    annotation(Placement(transformation(extent={{-140,40},{-100,80}})));

  Modelica.Blocks.Interfaces.RealInput T_outdoor(unit="K")
    "Outdoor dry-bulb temperature in Kelvin (replaces TMY3 weather reader)"
    annotation(Placement(transformation(extent={{-140,-80},{-100,-40}})));

  // ===== Outputs =====
  Modelica.Blocks.Interfaces.RealOutput P_cooling(unit="W")
    "Total cooling system electrical power (compressor + fans) in Watts"
    annotation(Placement(transformation(extent={{100,60},{140,100}})));

  Modelica.Blocks.Interfaces.RealOutput T_indoor(unit="K")
    "Data center room air temperature in Kelvin"
    annotation(Placement(transformation(extent={{100,-20},{140,20}})));

  Modelica.Blocks.Interfaces.RealOutput COP
    "System COP: Q_cooling / P_cooling"
    annotation(Placement(transformation(extent={{100,-100},{140,-60}})));

  // ===== Room Parameters (from Buildings example: 50x40x3 m) =====
  parameter Real rooLen = 50 "Room length (m)";
  parameter Real rooWid = 40 "Room width (m)";
  parameter Real rooHei = 3 "Room height (m)";
  parameter Real T_setpoint = 298.15
    "Room air temperature setpoint (25 degC)";
  parameter Real T_supply_set = 291.13
    "Supply air temperature setpoint (18 degC)";

  // ===== DX Coil Parameters (from Buildings example: 4-stage, COP_nom=3) =====
  parameter Real COP_nominal = 3.0
    "Nominal COP at rating conditions (from Buildings DX coil data)";
  parameter Real T_condenser_nom = 308.15
    "Nominal outdoor (condenser) temperature for COP rating (35 degC)";
  parameter Real COP_temp_coeff = 0.025
    "COP degradation per K above T_condenser_nom";
  parameter Real COP_plr_coeff = 0.1
    "COP improvement at part load due to cycling";
  parameter Real Q_coil_nominal = 1000000
    "Nominal cooling coil capacity (W), = 2 * 500kW IT load";

  // ===== Fan Parameters (from Buildings example) =====
  parameter Real m_air_nominal = 82.84
    "Nominal air mass flow rate (kg/s)";
  parameter Real dp_fan_nominal = 500
    "Fan design pressure rise (Pa)";
  parameter Real eta_fan = 0.7 "Fan total efficiency";
  parameter Real minSpeFan = 0.2 "Minimum fan speed ratio";

  // ===== Economizer Parameters (from Buildings example) =====
  parameter Real T_eco_high = 291.13
    "Outdoor temp above which economizer closes (K)";
  parameter Real T_eco_low = 286.15
    "Outdoor temp below which full free cooling (K)";
  parameter Real minOAFra = 0.05 "Minimum outdoor air fraction";

  // ===== Physical constants =====
  parameter Real rho_air = 1.2 "Air density (kg/m3)";
  parameter Real cp_air = 1006 "Air specific heat (J/(kg*K))";

  // ===== Internal Variables =====
  Real V_room "Room volume (m3)";
  Real C_room "Room thermal capacitance (J/K)";
  Real eco_fraction "Economizer outdoor air fraction (0-1)";
  Real T_mixed "Mixed air temperature after economizer (K)";
  Real Q_free "Free cooling capacity from economizer (W)";
  Real Q_mech_needed "Remaining load for DX compressor (W)";
  Real PLR "Part load ratio of DX coil (0-1)";
  Real COP_actual "Temperature-adjusted COP";
  Real P_compressor "DX compressor electrical power (W)";
  Real fan_speed "Fan speed ratio (0-1)";
  Real P_fan "Fan electrical power (W)";
  Real cooling_mode "1=free, 2=partial mechanical, 3=full mechanical";

initial equation
  T_indoor = T_setpoint;

equation
  // Room geometry and thermal mass
  V_room = rooLen * rooWid * rooHei;
  C_room = rho_air * V_room * cp_air * 5;

  // --- Cooling Mode Selection (from Buildings CoolingMode controller) ---
  cooling_mode = if T_outdoor < T_eco_low then 1.0
                 elseif T_outdoor < T_eco_high then 2.0
                 else 3.0;

  // --- Airside Economizer ---
  eco_fraction = if cooling_mode < 1.5 then 1.0
                 elseif cooling_mode < 2.5 then max(minOAFra, min(1.0,
                   (T_eco_high - T_outdoor) / max(0.1, T_eco_high - T_eco_low)))
                 else minOAFra;

  // Mixed air temperature
  T_mixed = eco_fraction * T_outdoor + (1 - eco_fraction) * T_indoor;

  // Free cooling capacity
  Q_free = m_air_nominal * cp_air * max(0, T_indoor - T_mixed);

  // Mechanical cooling needed after economizer
  Q_mech_needed = max(0, Q_IT - Q_free);

  // --- DX Compressor ---
  PLR = min(1.0, Q_mech_needed / max(1, Q_coil_nominal));

  COP_actual = max(1.0,
    COP_nominal
    * (1 - COP_temp_coeff * max(0, T_outdoor - T_condenser_nom))
    * (1 + COP_plr_coeff * (1 - PLR)));

  P_compressor = if cooling_mode < 1.5 then 0
                 else Q_mech_needed / max(1, COP_actual);

  // --- Fan (affinity laws: power ~ speed^3) ---
  fan_speed = max(minSpeFan, min(1.0, Q_IT / max(1, Q_coil_nominal)));

  P_fan = (m_air_nominal / rho_air) * dp_fan_nominal / eta_fan
          * fan_speed * fan_speed * fan_speed;

  // --- Room thermal dynamics ---
  C_room * der(T_indoor) = Q_IT - Q_free - Q_mech_needed;

  // --- Total outputs ---
  P_cooling = max(0, P_compressor + P_fan);

  COP = if P_cooling > 10 then (Q_free + Q_mech_needed) / P_cooling
        else COP_nominal;

  annotation(
    Documentation(info="<html>
      <p>Physics-based DX-cooled data center cooling model for FMU co-simulation,
      based on the LBL Buildings Library DXCooledAirsideEconomizer example.</p>
      <h4>Key physics</h4>
      <ul>
        <li>Three cooling modes: free cooling, partial mechanical, full mechanical</li>
        <li>Airside economizer with outdoor air mixing</li>
        <li>Variable-speed DX compressor with temperature-dependent COP (nominal=3.0)</li>
        <li>Fan power following affinity laws (cubic speed relation)</li>
        <li>Room thermal mass (50x40x3m data center room)</li>
      </ul>
    </html>"),
    experiment(StopTime=86400, Interval=1)
  );
end DXCooledAirsideEconomizer;
